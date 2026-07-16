package agentcore

import (
	"runtime/debug"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// The streaming bridge: forwards a round's fantasy streaming callbacks into the
// Observer (SSE for interactive, session log for scheduled) AND accumulates a
// neutral, mode-agnostic record of every reasoning / text / tool_call /
// tool_result the round produced.
//
// This is the part chat's session.go::RunTurn owned — wiring fantasy's
// OnTextDelta / OnReasoning* / OnToolCall / OnToolResult to an EventSink while
// building the persisted history. P6a deferred it; this file restores it INSIDE
// the unified loop so BOTH modes get live event forwarding and an accumulated
// history with no per-driver fork.
//
// The accumulated entries use a neutral RunEntry type (not agent.HistoryEntry)
// so agentcore stays import-cycle-free; the interactive driver maps RunEntry →
// agent.HistoryEntry at the boundary.

// RunEntry is one neutral history/event record accumulated during a run. It
// mirrors the agent.HistoryEntry shape (role/type/typed content) but lives in
// agentcore so the loop can build it without importing the driver package.
//
// Type is one of: "reasoning", "text", "tool_call", "tool_result". The driver
// maps these onto its own persisted HistoryEntry types verbatim.
type RunEntry struct {
	Role string // "user" | "assistant" | "tool"
	Type string // "reasoning" | "text" | "tool_call" | "tool_result"

	// Text carries the body for reasoning / text / tool_result entries.
	Text string

	// Tool-call / tool-result fields (empty for reasoning/text).
	ToolCallID string
	ToolName   string
	ToolInput  string // raw JSON the model emitted (tool_call only)
	IsErr      bool   // tool_result only
}

// SSE event-payload field keys, matching the names the chat frontend reads off
// each event (kept in sync with the tool.call / tool.result / text.delta /
// reasoning.* handlers in chat-experience.tsx).
const (
	evtFieldID    = "id"
	evtFieldName  = "name"
	evtFieldInput = "input"
	evtFieldText  = "text"
	evtFieldIsErr = "is_err"

	// Context-pressure event payload fields (#209).
	evtFieldUsedTokens    = "used_tokens"
	evtFieldWindowSize    = "window_size"
	evtFieldPct           = "pct"
	evtFieldRemovedTurns  = "removed_turns"
	evtFieldSummaryTokens = "summary_tokens"
)

// Context-window pressure SSE event names (#209). Emitted from the enforcement
// loop before a round's stream — a warning as the prompt nears the model's
// context window, and an informational marker when older history is proactively
// summarized to make room. The chat frontend renders each as a non-blocking
// banner (see chat-experience.tsx).
const (
	evtContextPressure  = "fleet.context_pressure"
	evtContextCompacted = "fleet.context_compacted"
)

// toolResultMaxStreamBytes bounds the tool-result text forwarded to the
// Observer (the full untruncated text is still accumulated for persistence).
const toolResultMaxStreamBytes = 4000

// streamSink owns the per-run accumulation of streamed events. It is safe for
// concurrent calls from fantasy's callback goroutines: the callbacks fire from
// the streaming reader while tool execution may run in parallel.
//
// It forwards each event to the Observer (when one is wired) AND appends a
// neutral RunEntry, so the loop ends with both a live-streamed UI and a
// persistable history of the round's reasoning/text/tool work.
type streamSink struct {
	observer Observer

	mu sync.Mutex
	// entries is the ordered accumulation of this run's reasoning / text /
	// tool_call / tool_result records.
	entries []RunEntry
	// finalText accumulates the assistant's user-visible text across the run so
	// the loop can recover it for the finalize hook + the Result.
	finalText strings.Builder
	// reasoningBufs buffers reasoning deltas per id; committed on End.
	reasoningBufs map[string]*strings.Builder
	// toolEvents counts observable tool side effects (tool_call/tool_result).
	// The resilience layer compares it against a per-attempt mark to decide
	// whether a failed stream attempt may be re-driven: text/reasoning output is
	// regenerable, an executed tool is not (re-driving the round could repeat
	// its side effect). See streamRoundWithResilience.
	toolEvents int
}

func newStreamSink(obs Observer) *streamSink {
	return &streamSink{
		observer:      obs,
		reasoningBufs: make(map[string]*strings.Builder),
	}
}

// emit forwards an event to the Observer when one is wired (nil-safe).
//
// A panic in the Observer is CONTAINED here (#795): these callbacks fire inside
// fantasy's unsupervised tool-exec goroutines, where an escaping panic would
// kill the process. Record-then-degrade — the event is dropped and the run
// continues; observation is best-effort, tool execution is not.
func (s *streamSink) emit(eventType string, payload map[string]any) {
	if s.observer == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			safe.EmitPanic("agentcore.observer."+eventType, r, debug.Stack())
		}
	}()
	s.observer.Observe(eventType, payload)
}

// onTextDelta forwards a text chunk to the Observer and accumulates it.
func (s *streamSink) onTextDelta(text string) {
	s.mu.Lock()
	s.finalText.WriteString(text)
	s.mu.Unlock()
	s.emit("text.delta", map[string]any{evtFieldText: text})
}

// onReasoningStart begins a reasoning block; some providers ship the whole
// block on the start event (Gemini), others stream deltas — capture both.
func (s *streamSink) onReasoningStart(id, text string) {
	s.mu.Lock()
	b := &strings.Builder{}
	if text != "" {
		b.WriteString(text)
	}
	s.reasoningBufs[id] = b
	s.mu.Unlock()
	s.emit("reasoning.start", map[string]any{evtFieldID: id, evtFieldText: text})
}

func (s *streamSink) onReasoningDelta(id, text string) {
	s.mu.Lock()
	if b := s.reasoningBufs[id]; b != nil {
		b.WriteString(text)
	}
	s.mu.Unlock()
	s.emit("reasoning.delta", map[string]any{evtFieldID: id, evtFieldText: text})
}

// onReasoningEnd commits the reasoning block as a RunEntry. Prefers fantasy's
// accumulated content (start + deltas), falling back to our local buffer.
func (s *streamSink) onReasoningEnd(id, content string) {
	reasoning := strings.TrimSpace(content)
	s.mu.Lock()
	if reasoning == "" {
		if b, ok := s.reasoningBufs[id]; ok {
			reasoning = strings.TrimSpace(b.String())
		}
	}
	delete(s.reasoningBufs, id)
	if reasoning != "" {
		s.entries = append(s.entries, RunEntry{Role: roleAssistant, Type: "reasoning", Text: reasoning})
	}
	s.mu.Unlock()
	s.emit("reasoning.end", map[string]any{evtFieldID: id, evtFieldText: reasoning})
}

// onToolCall forwards + records an assistant tool call.
func (s *streamSink) onToolCall(id, name, input string) {
	s.mu.Lock()
	s.toolEvents++
	s.entries = append(s.entries, RunEntry{
		Role: roleAssistant, Type: "tool_call",
		ToolCallID: id, ToolName: name, ToolInput: input,
	})
	s.mu.Unlock()
	s.emit("tool.call", map[string]any{evtFieldID: id, evtFieldName: name, evtFieldInput: input})
}

// onToolResult forwards (truncated) + records (full) a tool result.
func (s *streamSink) onToolResult(id, name, text string, isErr bool) {
	// Backstop redaction for the observer/SSE + recorded-entry path. The tool
	// wrappers already redact resp.Content, but pre-gated tools register verbatim
	// and reach here directly — so scrub again before anything is recorded,
	// streamed to the browser, or persisted to turn_events.
	text = toolRedactor().Redact(text)
	s.mu.Lock()
	s.toolEvents++
	s.entries = append(s.entries, RunEntry{
		Role: roleTool, Type: "tool_result",
		ToolCallID: id, ToolName: name, Text: text, IsErr: isErr,
	})
	s.mu.Unlock()
	s.emit("tool.result", map[string]any{
		evtFieldID:    id,
		evtFieldName:  name,
		evtFieldText:  truncate(text, toolResultMaxStreamBytes),
		evtFieldIsErr: isErr,
	})
}

// toolEventCount is nil-safe (0): the resilience loop tolerates a nil sink
// (the lifted parity tests build rounds without one).
func (s *streamSink) toolEventCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.toolEvents
}

// sinkMark captures the sink's accumulation point at the start of a stream
// attempt so a failed attempt's partial output can be rolled back before the
// attempt is re-driven (in-place stream-blip retry or fallback swap). The
// re-driven attempt regenerates the round from scratch — without the rollback
// its output would be appended AFTER the failed attempt's partial text,
// duplicating it in both the persisted history and the final answer.
type sinkMark struct {
	entries    int
	finalText  int
	toolEvents int
}

// mark snapshots the current accumulation point. Nil-safe (zero mark).
func (s *streamSink) mark() sinkMark {
	if s == nil {
		return sinkMark{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return sinkMark{entries: len(s.entries), finalText: s.finalText.Len(), toolEvents: s.toolEvents}
}

// rollbackTo discards everything accumulated after the mark: partial entries,
// partial final text, and any in-flight (uncommitted) reasoning buffers. Only
// called on a failed attempt that produced NO tool events past the mark — the
// resilience layer suppresses recovery otherwise — so no tool_call/tool_result
// entries are ever dropped, and no tool execution can be racing the rollback.
// Events already forwarded to the Observer cannot be recalled; the observer
// stream shows the abandoned partial followed by the retry note, while the
// accumulated history (what persists) stays clean. Nil-safe (no-op).
func (s *streamSink) rollbackTo(m sinkMark) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) > m.entries {
		s.entries = s.entries[:m.entries]
	}
	if s.finalText.Len() > m.finalText {
		kept := s.finalText.String()[:m.finalText]
		s.finalText.Reset()
		s.finalText.WriteString(kept)
	}
	s.toolEvents = m.toolEvents
	clear(s.reasoningBufs)
}

// snapshot returns a copy of the accumulated entries plus the accumulated final
// text. Safe to call after the run completes.
func (s *streamSink) snapshot() ([]RunEntry, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RunEntry, len(s.entries))
	copy(out, s.entries)
	return out, s.finalText.String()
}

// toolResultText flattens a fantasy ToolResultContent into the (text, isErr)
// pair we forward + persist. Shared with the finalize hook.
func toolResultText(tr fantasy.ToolResultContent) (string, bool) {
	if tr.Result == nil {
		return "", false
	}
	if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
		return txt.Text, false
	}
	if errv, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result); ok {
		if errv.Error != nil {
			return errv.Error.Error(), true
		}
		return "", true
	}
	return "", false
}
