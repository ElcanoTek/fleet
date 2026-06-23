// Package fakellm implements a small, wire-compatible fake of the OpenRouter
// chat-completions API for deterministic end-to-end testing.
//
// It speaks the exact OpenAI/OpenRouter chat-completions wire format the fantasy
// library (and the underlying charmbracelet/openai-go SDK) expects, including
//
//   - POST /api/v1/chat/completions with stream:true SSE responses,
//   - assistant tool_calls deltas (function name + JSON arguments),
//   - a terminal choice carrying a non-empty finish_reason (required: fantasy
//     treats finish_reason:"" with no tool calls as a truncated stream and
//     retries), and a final usage-only chunk, then data: [DONE],
//   - GET /api/v1/models returning a tiny canned catalogue.
//
// Fleet is pointed at it by exporting OPENROUTER_BASE_URL=<server URL> (see
// internal/agentcore/provider.go). The real provider, SSE parser, tool loop,
// scheduler, worker pool and Podman sandbox all run unchanged; only the LLM
// origin is swapped, so a spec can drive a genuine multi-turn tool loop.
//
// # Scripting
//
// The server is driven by Scenarios keyed by a marker the caller embeds in the
// prompt: the text "[[scenario:NAME]]" anywhere in any message selects the
// scenario NAME. A Scenario is an ordered list of Steps; the server picks the
// step matching the current turn index, which it derives deterministically from
// the conversation so far (the number of assistant turns already present in the
// request). So:
//
//	turn 0 → Step 0 (e.g. emit a bash tool_call),
//	fleet runs bash in the real sandbox, appends the result, re-calls →
//	turn 1 → Step 1 (e.g. emit a run_python tool_call),
//	fleet runs python, re-calls →
//	turn 2 → Step 2 (final assistant text).
//
// Steps can also inject failures (429/500/malformed/timeout) for resilience
// coverage. When no marker matches, DefaultScenario is used.
//
// For the common "give this conversation a distinct, predictable reply" case
// there is a lighter seam than a scenario: an "[[echo:TEXT]]" marker makes every
// request carrying it stream back exactly TEXT, turn-independently. Two chats
// seeded with different echoes get different first-assistant replies — and so
// different sidebar titles — which the live conversation-management specs rely
// on to tell conversations apart.
package fakellm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// StepKind enumerates what a single scripted model turn does.
type StepKind string

const (
	// StepToolCalls emits one or more assistant tool_calls and finishes with
	// finish_reason:"tool_calls", so fleet executes the tools and re-calls.
	StepToolCalls StepKind = "tool_calls"
	// StepText emits assistant text and finishes with finish_reason:"stop",
	// ending the agent turn.
	StepText StepKind = "text"
	// StepStatus replies with a non-2xx HTTP status (e.g. 429, 500) before any
	// streaming, exercising fleet's retry/backoff path.
	StepStatus StepKind = "status"
	// StepMalformed streams a body that is not valid SSE/JSON, exercising the
	// parser's error handling.
	StepMalformed StepKind = "malformed"
	// StepTimeout sleeps past any reasonable client deadline without writing a
	// terminal chunk, exercising fleet's timeout handling. The sleep is bounded
	// by the request context so the server never leaks a goroutine.
	StepTimeout StepKind = "timeout"
)

// ToolCall is one assistant tool invocation to emit in a StepToolCalls.
type ToolCall struct {
	// ID is the tool_call_id fleet echoes back on the tool result message. A
	// stable, unique value per call keeps the loop unambiguous.
	ID string
	// Name is the tool to invoke (e.g. "bash", "run_python"). Must match a tool
	// fleet registers for the turn.
	Name string
	// Arguments is the JSON-encoded argument object for the tool (a string, per
	// the OpenAI wire format).
	Arguments string
}

// Step is one scripted model turn.
type Step struct {
	Kind StepKind
	// Text is the assistant message for StepText.
	Text string
	// ToolCalls are the calls to emit for StepToolCalls.
	ToolCalls []ToolCall
	// Status is the HTTP status code for StepStatus (e.g. 429, 500).
	Status int
	// StatusBody is the optional response body for StepStatus.
	StatusBody string
	// Delay sleeps before the step responds (StepTimeout uses it as the sleep
	// duration; other kinds may use it to simulate latency).
	Delay time.Duration
}

// Scenario is an ordered list of steps, one per model turn.
type Scenario struct {
	Steps []Step
}

// scenarioMarker matches "[[scenario:NAME]]" in a prompt. NAME is any run of
// non-"]" characters.
var scenarioMarker = regexp.MustCompile(`\[\[scenario:([^\]]+)\]\]`)

// echoMarker matches "[[echo:TEXT]]" in a prompt. Unlike a scenario (which is
// turn-indexed and shared), an echo is a deterministic, turn-INDEPENDENT reply:
// every completion request whose messages contain the marker streams back
// exactly TEXT as the assistant text. This gives a spec a stable, DISTINCT
// reply per conversation without registering a scenario — useful when a journey
// needs to tell two conversations apart (e.g. the sidebar titles each chat from
// its first assistant reply, so two distinct echoes yield two distinct titles).
// The last marker in the message list wins, so an edited/resent turn echoes its
// new text. An echo marker takes precedence over any scenario marker.
var echoMarker = regexp.MustCompile(`\[\[echo:([^\]]+)\]\]`)

// Server is the configured fake. It is safe for concurrent use.
type Server struct {
	mu        sync.RWMutex
	scenarios map[string]Scenario
	defaultSc Scenario
	models    []string

	// hits counts requests per scenario for assertions/debugging.
	hits map[string]int
}

// New returns a Server with a sensible DefaultScenario (single canned reply)
// and a tiny model catalogue. Register more scenarios with Scenario().
func New() *Server {
	return &Server{
		scenarios: map[string]Scenario{},
		defaultSc: Scenario{Steps: []Step{{
			Kind: StepText,
			Text: "fake-llm default reply.",
		}}},
		models: []string{
			"anthropic/claude-opus-4.8",
			"anthropic/claude-sonnet-4.6",
			"moonshotai/kimi-k2.6",
			"google/gemini-3.5-flash",
			"openai/gpt-5.2",
		},
		hits: map[string]int{},
	}
}

// Scenario registers (or replaces) a named scenario. Returns the server for
// chaining.
func (s *Server) Scenario(name string, sc Scenario) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios[name] = sc
	return s
}

// SetDefault overrides the fallback scenario used when no marker matches.
func (s *Server) SetDefault(sc Scenario) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultSc = sc
	return s
}

// Hits returns how many chat-completions requests selected the given scenario.
func (s *Server) Hits(name string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hits[name]
}

// Handler returns the http.Handler. Mount it on any server/listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/api/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// ── chat-completions wire types (request side; only the fields we read) ──

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body", http.StatusBadRequest)
		return
	}

	// An echo marker short-circuits scenario selection with a deterministic,
	// turn-independent reply (see echoMarker). This keeps simple "give this
	// conversation a distinct, predictable reply" journeys from needing a
	// registered scenario.
	if text, ok := echoText(req.Messages); ok {
		s.mu.Lock()
		s.hits["__echo__"]++
		s.mu.Unlock()
		if !req.Stream {
			s.jsonText(w, req.Model, text)
			return
		}
		s.streamText(w, r, req.Model, text, 0)
		return
	}

	name, sc := s.selectScenario(req.Messages)
	s.mu.Lock()
	s.hits[name]++
	s.mu.Unlock()

	turn := assistantTurns(req.Messages)
	step := stepForTurn(sc, turn)

	switch step.Kind {
	case StepStatus:
		if step.Delay > 0 {
			sleepCtx(r, step.Delay)
		}
		code := step.Status
		if code == 0 {
			code = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		bodyText := step.StatusBody
		if bodyText == "" {
			bodyText = fmt.Sprintf(`{"error":{"message":"fake-llm injected %d","type":"server_error","code":%d}}`, code, code)
		}
		_, _ = io.WriteString(w, bodyText)
		return
	case StepMalformed:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Not valid SSE/JSON: a bare data line with garbage and no terminator.
		_, _ = io.WriteString(w, "data: {not-json[[[\n\n")
		flush(w)
		return
	case StepTimeout:
		d := step.Delay
		if d == 0 {
			d = 60 * time.Second
		}
		// Begin a stream then stall; bounded by the request context so we never
		// leak. fleet's per-call deadline fires first.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush(w)
		sleepCtx(r, d)
		return
	case StepToolCalls:
		if !req.Stream {
			s.jsonToolCalls(w, req.Model, step.ToolCalls)
			return
		}
		s.streamToolCalls(w, r, req.Model, step.ToolCalls, step.Delay)
		return
	default: // StepText (and any unrecognized kind falls back to text)
		if !req.Stream {
			s.jsonText(w, req.Model, step.Text)
			return
		}
		s.streamText(w, r, req.Model, step.Text, step.Delay)
		return
	}
}

// selectScenario finds the first "[[scenario:NAME]]" marker across all message
// contents and returns the matching scenario; falls back to the default.
func (s *Server) selectScenario(msgs []chatMessage) (string, Scenario) {
	for _, m := range msgs {
		text := contentText(m.Content)
		if mt := scenarioMarker.FindStringSubmatch(text); mt != nil {
			name := strings.TrimSpace(mt[1])
			s.mu.RLock()
			sc, ok := s.scenarios[name]
			s.mu.RUnlock()
			if ok {
				return name, sc
			}
			// Marker named an unknown scenario: surface it as a distinct key so
			// a spec sees the miss rather than silently getting the default.
			return name, s.defaultScenario()
		}
	}
	return "__default__", s.defaultScenario()
}

// echoText returns the text of the LAST "[[echo:TEXT]]" marker across all
// message contents, if any. Last-wins so an edited/resent user turn echoes its
// new text rather than the original. The returned text is trimmed of
// surrounding whitespace.
func echoText(msgs []chatMessage) (string, bool) {
	var found string
	var ok bool
	for _, m := range msgs {
		text := contentText(m.Content)
		for _, mt := range echoMarker.FindAllStringSubmatch(text, -1) {
			found = strings.TrimSpace(mt[1])
			ok = true
		}
	}
	return found, ok
}

func (s *Server) defaultScenario() Scenario {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultSc
}

// stepForTurn returns the step for the given turn index, clamping to the last
// step (so a longer-than-scripted loop keeps replying with the final step
// rather than panicking).
func stepForTurn(sc Scenario, turn int) Step {
	if len(sc.Steps) == 0 {
		return Step{Kind: StepText, Text: "fake-llm: empty scenario."}
	}
	if turn >= len(sc.Steps) {
		return sc.Steps[len(sc.Steps)-1]
	}
	return sc.Steps[turn]
}

// assistantTurns counts how many assistant messages are already in the request.
// fantasy appends one assistant message per completed model turn before
// re-calling, so this is the deterministic 0-based index of the turn the model
// is now producing.
func assistantTurns(msgs []chatMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// contentText extracts plain text from a message content value, which may be a
// JSON string or an array of content parts ({type:"text",text:"..."}).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	return ""
}
