package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Sub-agent progress streaming (#1043 follow-up).
//
// A delegation used to be a black box for its entire lifetime: the parent's
// stream carried the spawn_subagent tool CALL (its task text) and then nothing
// at all until the child returned — minutes later, or never, if the child ran
// out of iterations. The operator watching chat saw a spinner over the task
// text and had no way to tell a working child from a stuck one, and no way to
// see which tool the child was on.
//
// The fix keeps the one governed core untouched: the CHILD's ordinary run
// Observer (the same one that writes its captain's log) is tee'd into a
// forwarder that RELABELS each child event as a single `subagent.progress`
// event on the PARENT's observer. Nothing new is executed, no second event
// path exists — the events already existed, they were simply not attributed
// and not forwarded. The parent tool-call id rides every event so a UI can
// attach the progress to the exact spawn chip that produced it (a parent may
// fan out several children in one turn, and they run concurrently).
//
// Text and reasoning deltas are COALESCED (a child emits hundreds per answer):
// at most one preview event per subagentTextInterval, carrying the tail of what
// the child has written since. Tool calls/results are never coalesced — they
// are the steps an operator is actually watching for.

// SubagentProgressEvent is the single event name the forwarder emits on the
// parent's observer. One name (rather than started/step/finished events) keeps
// every consumer's handling to one branch, and any observer that does not know
// it — the captain's-log writer, the orchestrator log stream — ignores it.
const SubagentProgressEvent = "subagent.progress"

// Progress phases carried in the event's "phase" field.
const (
	subagentPhaseStarted  = "started"     // the child was built and is about to run
	subagentPhaseTool     = "tool"        // the child called a tool
	subagentPhaseToolDone = "tool_result" // that tool returned
	subagentPhaseText     = "text"        // the child is writing its answer
	subagentPhaseThinking = "thinking"    // the child is reasoning (extended thinking)
	subagentPhaseNote     = "note"        // the run loop nudged the child (enforcement)
	subagentPhaseFinished = "finished"    // terminal: spend, status, step count
)

// Coalescing + preview bounds. The previews exist to tell an operator WHAT the
// child is doing, not to mirror its output, so they stay small: a child's full
// transcript is one click away behind the child card's Transcript disclosure.
const (
	subagentTextInterval = 700 * time.Millisecond
	// A preview also needs something worth showing: a provider's first delta is
	// often a single character, and "writing: W" is noise, not progress.
	subagentMinPreviewChars = 24
	subagentPreviewChars    = 240
	subagentDetailChars     = 160
	subagentMaxToolsUsed    = 24
	subagentTaskPreviewCh   = 400
)

// childProgress forwards one child run's events to the parent's observer and
// accumulates the trail (step count + distinct tools) the spawn result reports
// back to the model. Safe for concurrent use: fantasy fires the streaming
// callbacks from its reader goroutine while tool execution runs in parallel.
type childProgress struct {
	parent     agentcore.Observer
	toolCallID string
	childID    string
	role       string

	mu        sync.Mutex
	steps     int
	toolsUsed []string
	// text buffers the child's un-emitted text/reasoning; lastEmit rate-limits
	// the preview events built from it.
	text     strings.Builder
	lastEmit time.Time
}

// newChildProgress returns a forwarder, or nil when nobody is watching the
// parent (tests, headless one-shots). A nil *childProgress is safe to use: every
// method below is nil-receiver tolerant, so the spawn path needs no branches.
func newChildProgress(parent agentcore.Observer, toolCallID, childID, role string) *childProgress {
	if parent == nil {
		return nil
	}
	// lastEmit starts at construction, not the zero time: a zero would make the
	// FIRST delta look overdue and emit a one-character "preview" before the
	// child has written anything worth showing.
	return &childProgress{
		parent: parent, toolCallID: toolCallID, childID: childID, role: role,
		lastEmit: time.Now(),
	}
}

// observer returns the forwarder as an agentcore.Observer, or nil when there is
// nothing to forward to (a typed-nil Observer would defeat the child's own nil
// check in runObserver).
func (p *childProgress) observer() agentcore.Observer {
	if p == nil {
		return nil
	}
	return p
}

// emit sends one progress event with the identity fields every phase carries.
func (p *childProgress) emit(phase string, fields map[string]any) {
	if p == nil || p.parent == nil {
		return
	}
	payload := map[string]any{
		"tool_call_id":     p.toolCallID,
		"child_session_id": p.childID,
		"role":             p.role,
		"phase":            phase,
	}
	for k, v := range fields {
		payload[k] = v
	}
	p.parent.Observe(SubagentProgressEvent, payload)
}

// started announces the child before its first model call, so the UI can swap
// the spawn chip from "queued" to a live card the moment the run begins.
func (p *childProgress) started(task, workdir, model string) {
	p.emit(subagentPhaseStarted, map[string]any{
		"task":    truncateRunes(task, subagentTaskPreviewCh),
		"workdir": workdir,
		"model":   model,
	})
}

// finished is the terminal event: status, spend, and the step trail. Emitted by
// the spawn tool (not by the child's loop) so it also covers a child that died
// on an error or a timeout.
func (p *childProgress) finished(success bool, usage agentcore.RunUsage, elapsed time.Duration, note string) {
	steps, toolsUsed := p.snapshot()
	p.emit(subagentPhaseFinished, map[string]any{
		"success":     success,
		"cost_usd":    usage.CostUSD,
		"tokens":      usage.PromptTokens + usage.CompletionTokens,
		"steps":       steps,
		"tools_used":  toolsUsed,
		"duration_ms": elapsed.Milliseconds(),
		"note":        note,
	})
}

// Observe implements agentcore.Observer over the CHILD's run events. It never
// mutates or blocks the child's run — it relabels and forwards.
func (p *childProgress) Observe(eventType string, payload map[string]any) {
	if p == nil {
		return
	}
	switch eventType {
	case "tool.call":
		name, _ := payload["name"].(string)
		if name == "" {
			return
		}
		p.flushText()
		step := p.recordTool(name)
		input, _ := payload["input"].(string)
		p.emit(subagentPhaseTool, map[string]any{
			"tool":   name,
			"step":   step,
			"detail": summarizeToolInput(input),
		})
	case "tool.result":
		name, _ := payload["name"].(string)
		isErr, _ := payload["is_err"].(bool)
		text, _ := payload["text"].(string)
		p.emit(subagentPhaseToolDone, map[string]any{
			"tool":   name,
			"step":   p.stepCount(),
			"is_err": isErr,
			"detail": truncateRunes(collapseWhitespace(text), subagentDetailChars),
		})
	case "text.delta":
		if t, _ := payload["text"].(string); t != "" {
			p.appendText(t, subagentPhaseText)
		}
	case "reasoning.delta", "reasoning.start":
		if t, _ := payload["text"].(string); t != "" {
			p.appendText(t, subagentPhaseThinking)
		}
	case "enforcement":
		if msg, _ := payload["message"].(string); msg != "" {
			p.flushText()
			p.emit(subagentPhaseNote, map[string]any{
				"detail": truncateRunes(collapseWhitespace(msg), subagentDetailChars),
			})
		}
	}
}

// recordTool counts a child step and remembers the tool name (deduped, in
// first-call order, bounded so a long-running child cannot grow the trail
// without limit). Returns the new step number.
func (p *childProgress) recordTool(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps++
	if len(p.toolsUsed) < subagentMaxToolsUsed {
		for _, existing := range p.toolsUsed {
			if existing == name {
				return p.steps
			}
		}
		p.toolsUsed = append(p.toolsUsed, name)
	}
	return p.steps
}

func (p *childProgress) stepCount() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.steps
}

// snapshot returns the accumulated trail for the terminal event + the tool
// result the model reads. Nil-safe (zero trail).
func (p *childProgress) snapshot() (int, []string) {
	if p == nil {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.toolsUsed))
	copy(out, p.toolsUsed)
	return p.steps, out
}

// appendText buffers a text/reasoning delta and emits a preview at most once per
// subagentTextInterval. The preview is the TAIL of the buffer (what the child is
// writing right now), which is what an operator watching a live card wants.
func (p *childProgress) appendText(text, phase string) {
	p.mu.Lock()
	p.text.WriteString(text)
	if time.Since(p.lastEmit) < subagentTextInterval || p.text.Len() < subagentMinPreviewChars {
		p.mu.Unlock()
		return
	}
	p.lastEmit = time.Now()
	preview := tailRunes(collapseWhitespace(p.text.String()), subagentPreviewChars)
	p.text.Reset()
	p.mu.Unlock()
	if preview != "" {
		p.emit(phase, map[string]any{"detail": preview})
	}
}

// flushText drops any buffered text without emitting: a tool call supersedes
// the "still writing" preview, and the child's real output is persisted in its
// own transcript regardless.
func (p *childProgress) flushText() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.text.Reset()
	p.mu.Unlock()
}

// summarizeToolInput renders a tool call's raw JSON arguments as one short
// human line ("query=…, limit=5") so the live card says what the child asked
// for, not just which tool it used. Falls back to the raw text (truncated) when
// the arguments are not a JSON object.
func summarizeToolInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return truncateRunes(collapseWhitespace(raw), subagentDetailChars)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	// Stable order: a map iteration would make the same call render differently
	// on every event.
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+truncateRunes(collapseWhitespace(scalarString(obj[k])), 60))
	}
	return truncateRunes(strings.Join(parts, ", "), subagentDetailChars)
}

// scalarString renders a JSON value compactly for the argument summary.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// collapseWhitespace flattens a multi-line blob to one line so a preview stays
// one line in the UI.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes bounds a preview by RUNES (not bytes) so a multi-byte character
// is never split into invalid UTF-8 on its way into a JSON event payload.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// tailRunes keeps the LAST max runes — the live edge of what the child is
// writing.
func tailRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}
