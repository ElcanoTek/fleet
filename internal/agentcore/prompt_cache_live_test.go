package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
)

// Live prompt-cache check: drives the real Run loop (fleet's own breakpoints,
// upstream pins, tail messages and tool roster) against OpenRouter and reports,
// per request, how much of the prompt the upstream served from cache. It costs
// real money, so it only runs with FLEET_CACHE_LIVE=1 and OPENROUTER_API_KEY
// set; FLEET_CACHE_LIVE_MODELS (comma-separated) picks the slugs, default the
// two compiled-in tiers. FLEET_CACHE_LIVE_TURN_GAP (a Go duration, default
// 5s) is the pause between user turns: a person never replies within a second,
// and implicit caches (Gemini 4) need a moment to become readable.
//
// The shape is a three-turn conversation whose second turn makes one tool
// call, built the way the interactive driver builds it (each user turn
// followed by its persisted runtime_today note, RuntimeDateInInput set — see
// agent.assembleTurnMessages), over a ~7K-token system prompt — past every family's minimum
// cacheable prefix (Gemini 4: 4,096). Turn 1 can only write the cache; every
// later request should read at least the system prompt + tools back.
//
//	FLEET_CACHE_LIVE=1 OPENROUTER_API_KEY=… go test -tags fleet_host_executor \
//	  -run TestPromptCacheLive -v ./internal/agentcore/
func TestPromptCacheLive(t *testing.T) {
	if os.Getenv("FLEET_CACHE_LIVE") != "1" {
		t.Skip("set FLEET_CACHE_LIVE=1 (and OPENROUTER_API_KEY) to run the live prompt-cache check")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("FLEET_CACHE_LIVE=1 needs OPENROUTER_API_KEY")
	}
	slugs := []string{DefaultCoreModel, DefaultMaxModel}
	if v := strings.TrimSpace(os.Getenv("FLEET_CACHE_LIVE_MODELS")); v != "" {
		slugs = strings.Split(v, ",")
	}
	for _, slug := range slugs {
		slug := strings.TrimSpace(slug)
		t.Run(strings.ReplaceAll(slug, "/", "_"), func(t *testing.T) {
			t.Parallel()
			runPromptCacheLive(t, key, slug)
		})
	}
}

// liveCacheSystemPrompt is a deterministic ~7K-token system prompt with a
// per-run nonce up front, so no earlier test run's cache can be hit and turn 1
// really is cold.
func liveCacheSystemPrompt(nonce string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a terse operations assistant. Session nonce: %s.\n\n", nonce)
	b.WriteString("Answer in one short sentence unless a tool is required. When the user asks to look up a record, call lookup_record exactly once, then answer from its result.\n\n## Reference handbook\n\n")
	for i := 1; i <= 160; i++ {
		fmt.Fprintf(&b, "%d. Policy %d: requests touching region %d must be logged with ticket prefix OPS-%04d, reviewed by the on-call lead within %d hours, and archived after %d days unless legal hold applies.\n", i, i, i%17, i*7, 1+i%12, 30+i%90)
	}
	return b.String()
}

type liveCacheRecord struct {
	Turn          int
	Provider      string
	PromptTokens  int
	CachedTokens  int
	CacheWrite    int
	Cost          float64
	Breakpoints   int
	ProviderField json.RawMessage
	Extra         map[string]json.RawMessage
}

// liveCacheTransport forwards to OpenRouter, keeps each request body, and
// pulls the final usage object (and the serving provider) out of the SSE
// stream AS THE CLIENT READS IT: the response body is teed, never buffered
// first, so the first event reaches fantasy as soon as it arrives and the
// first-chunk watchdog behaves exactly as in production.
type liveCacheTransport struct {
	mu      sync.Mutex
	turn    int
	records []liveCacheRecord
}

func (t *liveCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return resp, err
	}
	t.mu.Lock()
	turn := t.turn
	t.mu.Unlock()
	if dir := os.Getenv("FLEET_CACHE_LIVE_DUMP"); dir != "" {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(reqBody, &m)
		_ = os.WriteFile(fmt.Sprintf("%s/%s-%d.json", dir, strings.ReplaceAll(m.Model, "/", "_"), time.Now().UnixNano()), reqBody, 0o600)
	}
	resp.Body = &liveCacheBody{rc: resp.Body, finish: func(raw []byte) { t.record(turn, reqBody, raw) }}
	return resp, nil
}

// liveCacheBody tees the stream into a buffer and hands it to finish once,
// when the client hits EOF or closes the body (whichever comes first).
type liveCacheBody struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	once   sync.Once
	finish func([]byte)
}

func (b *liveCacheBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf.Write(p[:n])
	if err == io.EOF {
		b.once.Do(func() { b.finish(b.buf.Bytes()) })
	}
	return n, err
}

func (b *liveCacheBody) Close() error {
	b.once.Do(func() { b.finish(b.buf.Bytes()) })
	return b.rc.Close()
}

func (t *liveCacheTransport) record(turn int, reqBody, raw []byte) {
	rec := liveCacheRecord{Turn: turn, Extra: map[string]json.RawMessage{}}
	rec.Breakpoints = bytes.Count(reqBody, []byte(`"cache_control"`))
	var reqJSON struct {
		Provider json.RawMessage `json:"provider"`
	}
	_ = json.Unmarshal(reqBody, &reqJSON)
	rec.ProviderField = reqJSON.Provider
	for k, v := range extraTopLevelFields(reqBody) {
		rec.Extra[k] = v
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var chunk struct {
			Provider string `json:"provider"`
			Usage    *struct {
				PromptTokens        int     `json:"prompt_tokens"`
				Cost                float64 `json:"cost"`
				PromptTokensDetails struct {
					CachedTokens     int `json:"cached_tokens"`
					CacheWriteTokens int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk.Provider != "" {
			rec.Provider = chunk.Provider
		}
		if chunk.Usage != nil {
			rec.PromptTokens = chunk.Usage.PromptTokens
			rec.CachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			rec.CacheWrite = chunk.Usage.PromptTokensDetails.CacheWriteTokens
			rec.Cost = chunk.Usage.Cost
		}
	}
	t.mu.Lock()
	t.records = append(t.records, rec)
	t.mu.Unlock()
}

// extraTopLevelFields returns the request's top-level keys other than the
// ones every chat request carries, so the report shows routing/caching hints
// (provider, prompt_cache_key, user, …) fleet actually sent.
func extraTopLevelFields(body []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	for _, k := range []string{"model", "messages", "tools", "stream", "stream_options", "max_tokens", "temperature", "tool_choice", "provider", "usage"} {
		delete(m, k)
	}
	return m
}

func runPromptCacheLive(t *testing.T, key, slug string) {
	transport := &liveCacheTransport{}
	provider, err := openrouter.New(
		openrouter.WithAPIKey(key),
		openrouter.WithHTTPClient(&http.Client{Transport: transport, Timeout: 10 * time.Minute}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	model, err := provider.LanguageModel(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	system := liveCacheSystemPrompt(fmt.Sprintf("%s-%d", slug, time.Now().UnixNano()))
	lookup := fantasy.NewAgentTool("lookup_record", "Look up an operations record by its id.",
		func(_ context.Context, in struct {
			ID string `json:"id" description:"record id"`
		}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(fmt.Sprintf("record %s: status=open owner=ops-lead region=7", in.ID)), nil
		})

	userTurns := []string{
		"Which ticket prefix does policy 12 use?",
		"Look up record R-1042 and tell me its owner.",
		"How many hours does policy 30 allow for review?",
	}
	gap := 5 * time.Second
	if v := os.Getenv("FLEET_CACHE_LIVE_TURN_GAP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			gap = d
		}
	}
	// Per turn: the user message, its runtime_today note, and the replayed
	// assistant/tool messages.
	history := make([]fantasy.Message, 0, 4*len(userTurns))
	var fleetCached, fleetWrite int
	for i, u := range userTurns {
		if i > 0 {
			time.Sleep(gap)
		}
		transport.mu.Lock()
		transport.turn = i + 1
		transport.mu.Unlock()
		note, _ := RuntimeDateMessage(RuntimeToday(time.Now()))
		history = append(history, fantasy.NewUserMessage(u), note)
		session := NewLogSession()
		res, err := Run(ctx, ModeInteractive, RunConfig{
			EnvPrefix:          CanonicalEnvPrefix,
			NativeTools:        []fantasy.AgentTool{lookup},
			RuntimeDateInInput: true,
		}, Deps{
			Input:      historyInput{system: system, msgs: append([]fantasy.Message(nil), history...), label: "cache-live"},
			Policy:     newRoundsPolicy(session, 0),
			Executor:   &stubExecutor{},
			Model:      model,
			LogSession: session,
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		fleetCached += session.CachedTokens
		fleetWrite += session.CacheCreationTokens
		history = append(history, liveReplayEntries(res.Entries)...)
	}

	var wireCached, wireWrite, reads, laterReqs int
	var cost float64
	for i, r := range transport.records {
		t.Logf("%s req %d (turn %d) provider=%q prompt=%d cached=%d write=%d bp=%d cost=$%.5f pin=%s extra=%v",
			slug, i+1, r.Turn, r.Provider, r.PromptTokens, r.CachedTokens, r.CacheWrite, r.Breakpoints, r.Cost, r.ProviderField, keys(r.Extra))
		wireCached += r.CachedTokens
		wireWrite += r.CacheWrite
		cost += r.Cost
		if i > 0 {
			laterReqs++
			if r.CachedTokens > 0 {
				reads++
			}
		}
	}
	t.Logf("%s SUMMARY requests=%d later-requests-with-cache-read=%d/%d wire cached=%d write=%d | fleet-recorded cached=%d write=%d | cost=$%.4f",
		slug, len(transport.records), reads, laterReqs, wireCached, wireWrite, fleetCached, fleetWrite, cost)
	if fleetCached != wireCached {
		t.Errorf("%s: fleet recorded %d cached tokens, the wire reported %d", slug, fleetCached, wireCached)
	}
	// Known gap, logged rather than failed: fantasy's OpenRouter usage
	// struct has no cache_write_tokens field, so CacheCreationTokens stays 0
	// for every OpenRouter model. Billing is unaffected by default (fleet uses
	// OpenRouter's reported usage.cost); only per-model pricing overrides that
	// price cache writes under-count. See docs/PROMPT-CACHE-CONTRACT.md.
	if fleetWrite != wireWrite {
		t.Logf("%s: fleet recorded %d cache-write tokens, the wire reported %d (fantasy does not parse cache_write_tokens)", slug, fleetWrite, wireWrite)
	}
	// A family fleet sends explicit breakpoints to must read on every request
	// after the first. An implicit cache is the upstream's best effort — the
	// same request shape reads on one run and misses on the next (Gemini 4
	// Argon: 15/15 over five runs, then a turn-2 miss) — so one miss is
	// tolerated there. The deterministic guard for the request shape itself is
	// agent.TestTurnRequestIsAPrefixOfTheNext.
	allowed := 0
	if !supportsExplicitBreakpoints(slug) {
		allowed = 1
	}
	if laterReqs > 0 && reads < laterReqs-allowed {
		t.Errorf("%s: only %d of %d requests after the first read from cache (%d miss allowed)", slug, reads, laterReqs, allowed)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// liveReplayEntries turns a run's entries into the messages the next turn
// replays, in the shape agent.replayHistory produces (that package imports
// this one, so it cannot be called here): assistant text and tool calls
// coalesce into one assistant message, each followed by its tool results;
// reasoning is never replayed.
func liveReplayEntries(entries []RunEntry) []fantasy.Message {
	var out []fantasy.Message
	var pending []fantasy.MessagePart
	flush := func() {
		if len(pending) > 0 {
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: pending})
			pending = nil
		}
	}
	for _, e := range entries {
		switch e.Type {
		case "text":
			if e.Role == "assistant" {
				pending = append(pending, fantasy.TextPart{Text: e.Text})
			}
		case "tool_call":
			pending = append(pending, fantasy.ToolCallPart{ToolCallID: e.ToolCallID, ToolName: e.ToolName, Input: e.ToolInput})
		case "tool_result":
			flush()
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{ToolCallID: e.ToolCallID, Output: fantasy.ToolResultOutputContentText{Text: e.Text}},
			}})
		}
	}
	flush()
	return out
}
