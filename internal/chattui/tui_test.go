package chattui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestModelTurnLifecycle drives the model through a turn with synthetic messages
// (no real Program/terminal) and asserts the transcript + rendering behave: the
// user line is committed, streamed text + a tool call accumulate, and the
// completed turn commits an "agent" block carrying the reply. View() must not
// panic once sized.
func TestModelTurnLifecycle(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	// Size it (viewport needs dimensions before View()).
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)

	// Simulate sending (without the goroutine): commit the user line + stream.
	m.lastUser = "hello"
	m.history = append(m.history, stylePillUser.Render("you")+"\nhello")
	m.streaming = true
	m.applyEvent(Event{Name: "conversation", Data: map[string]any{"id": "conv-7"}})
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "bash"}})
	m.applyEvent(Event{Name: "text.delta", Data: map[string]any{"text": "draft I will now send"}})
	if m.convID != "conv-7" {
		t.Errorf("convID = %q, want conv-7", m.convID)
	}

	m.applyEvent(Event{Name: "text.delta", Data: map[string]any{"text": "the answer is **42**"}})
	m.applyEvent(Event{Name: "text.replace", Data: map[string]any{"text": "the answer is **42**"}})
	if got := m.assistant.String(); got != "the answer is **42**" {
		t.Fatalf("text.replace left superseded draft in the live buffer: %q", got)
	}

	m.finishTurn(turnDoneMsg{convID: "conv-7"})
	if m.streaming {
		t.Error("streaming should be false after finishTurn")
	}
	joined := strings.Join(m.history, "\n")
	if !strings.Contains(joined, "hello") {
		t.Error("user message missing from transcript")
	}
	if !strings.Contains(joined, "42") {
		t.Errorf("agent reply missing from transcript:\n%s", joined)
	}
	if strings.Contains(joined, "draft I will now send") {
		t.Errorf("committed transcript kept the retracted draft:\n%s", joined)
	}

	// View must render without panicking now that we're sized.
	_ = m.View()
}

func TestModelSlashCommands(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)

	m.convID = "abc"
	if cmd := m.runSlash("/new"); cmd != nil {
		t.Error("/new should not return a command")
	}
	if m.convID != "" {
		t.Errorf("/new should clear convID, got %q", m.convID)
	}

	m.runSlash("/model anthropic/claude-opus-4-8")
	if m.client.cfg.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("/model didn't set the model: %q", m.client.cfg.Model)
	}

	if !m.showReasoning {
		m.runSlash("/reasoning")
		if !m.showReasoning {
			t.Error("/reasoning should toggle reasoning display on")
		}
	}

	if cmd := m.runSlash("/quit"); cmd == nil {
		t.Error("/quit should return a quit command")
	}
}

// TestToolGlyphLifecycle pins the tool-line rendering contract: a call shows
// the running marker, a success flips to ✓, an error to ✗ — index-aligned
// even across multiple calls in one turn.
func TestToolGlyphLifecycle(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	m.streaming = true
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "bash"}})
	if len(m.toolLines) != 1 || !strings.Contains(m.toolLines[0], "bash") || !strings.Contains(m.toolLines[0], "running") {
		t.Fatalf("call line = %q", m.toolLines)
	}
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "run_python"}})
	m.applyEvent(Event{Name: "tool.result", Data: map[string]any{"is_err": true}})
	if !strings.Contains(m.toolLines[1], "✗ run_python") || !strings.Contains(m.toolLines[1], "failed") {
		t.Errorf("error result line = %q", m.toolLines[1])
	}
	if !strings.Contains(m.toolLines[0], "running") {
		t.Errorf("first tool should still be running: %q", m.toolLines[0])
	}
	// An unnamed call falls back to the generic label.
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{}})
	if !strings.Contains(m.toolLines[2], "⏺ tool") {
		t.Errorf("unnamed call line = %q", m.toolLines[2])
	}
}

// TestApprovalCardLifecycle drives a staged approval through the TUI: the
// tool.approval_required event queues a pending card and renders a transcript
// line, /approve settles the oldest one (against a fake server), and the
// resolution commits to the transcript. /deny on an empty queue is a no-op note.
func TestApprovalCardLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/approvals/appr-1") {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"status":"approved","result_text":"Scheduled task created. Task id: t-1"}`)
	}))
	defer srv.Close()

	m := newModel(Config{ServerURL: srv.URL, Email: "e@x.co"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)
	m.convID = "conv-1"

	m.streaming = true
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "appr-1",
		"tool":        "schedule_task",
		"summary":     map[string]any{"name": "nightly", "run_immediately": true},
		"frozen_args": map[string]any{"complete": true, "args": map[string]any{"name": "nightly"}},
	}})
	if len(m.pending) != 1 || m.pending[0].id != "appr-1" {
		t.Fatalf("pending = %+v", m.pending)
	}
	if joined := strings.Join(m.approvalLines, "\n"); !strings.Contains(joined, "needs approval") || !strings.Contains(joined, "nightly") {
		t.Errorf("approval transcript line missing: %q", joined)
	}
	// The approval line lives OUTSIDE toolLines so the staged call's
	// APPROVAL_REQUIRED tool.result can't clobber it into a ✗.
	if len(m.toolLines) != 0 {
		t.Errorf("approval must not enter the tool-line alignment: %q", m.toolLines)
	}

	// The status row advertises the pending card once the turn ends.
	m.streaming = false
	if out := m.render(); !strings.Contains(out, "1 approval pending") {
		t.Errorf("status row missing pending notice:\n%s", out)
	}

	cmd := m.runSlash("/approve")
	if cmd == nil {
		t.Fatal("/approve should return the resolve command")
	}
	if len(m.pending) != 0 {
		t.Error("/approve should dequeue the settled card")
	}
	msg, ok := cmd().(approvalResolvedMsg)
	if !ok {
		t.Fatalf("resolve cmd returned %T, want approvalResolvedMsg", msg)
	}
	m.finishApproval(msg)
	joined := strings.Join(m.history, "\n")
	if !strings.Contains(joined, "schedule_task approved") || !strings.Contains(joined, "Task id: t-1") {
		t.Errorf("approval outcome missing from transcript:\n%s", joined)
	}

	// /deny with an empty queue notes it instead of calling the server.
	m.runSlash("/deny")
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "no pending approvals") {
		t.Errorf("empty /deny note missing:\n%s", joined)
	}
}

// TestApprovalSupersededDropsPending pins the re-stage contract: a supersede
// event voids the older card for that tool so /approve can't settle a dead one.
func TestApprovalSupersededDropsPending(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{"approval_id": "a1", "tool": "send_email", "summary": map[string]any{}}})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{"approval_id": "a2", "tool": "schedule_task", "summary": map[string]any{}}})
	m.applyEvent(Event{Name: "tool.approval_superseded", Data: map[string]any{"tool": "send_email", "count": float64(1)}})
	if len(m.pending) != 1 || m.pending[0].id != "a2" {
		t.Errorf("pending after supersede = %+v, want only a2", m.pending)
	}
}

// TestNewConversationDropsPending: /new abandons the thread the cards belong
// to, so the queue must reset (they stay pending server-side).
func TestNewConversationDropsPending(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	m.convID = "conv-1"
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{"approval_id": "a1", "tool": "bash", "summary": map[string]any{}}})
	m.runSlash("/new")
	if len(m.pending) != 0 {
		t.Errorf("/new should drop pending cards, got %+v", m.pending)
	}
}

// TestStagedToolResultRendersPaused pins the live-observed sequence: a staged
// critical tool's tool.result arrives with is_err=true and an
// APPROVAL_REQUIRED: sentinel text — the line must read "awaiting approval",
// never "✗ failed".
func TestStagedToolResultRendersPaused(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	m.streaming = true
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "schedule_task"}})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "appr-1", "tool": "schedule_task", "summary": map[string]any{"name": "n"},
	}})
	m.applyEvent(Event{Name: "tool.result", Data: map[string]any{
		"name": "schedule_task", "is_err": true,
		"text": "APPROVAL_REQUIRED: the scheduled task has been staged for explicit user approval (approval_id=appr-1). Do NOT retry.",
	}})
	line := m.toolLines[0]
	if !strings.Contains(line, "⏸ schedule_task") || !strings.Contains(line, "awaiting approval") {
		t.Errorf("staged result line = %q, want a pause marker", line)
	}
	if strings.Contains(line, "✗") || strings.Contains(line, "failed") {
		t.Errorf("staged result must not render as failure: %q", line)
	}
	// …and a REAL failure still renders ✗.
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"name": "bash"}})
	m.applyEvent(Event{Name: "tool.result", Data: map[string]any{"name": "bash", "is_err": true, "text": "exit 1"}})
	if !strings.Contains(m.toolLines[1], "✗ bash") {
		t.Errorf("real failure line = %q", m.toolLines[1])
	}
}

// TestBarLine pins the header/status row layout math: left + right lay out to
// the full width, and a too-narrow terminal degrades to a single space gap
// instead of a negative repeat panic.
func TestBarLine(t *testing.T) {
	line := barLine(20, "ab", "cd")
	if lipglossWidth(line) != 20 {
		t.Errorf("width = %d, want 20 (%q)", lipglossWidth(line), line)
	}
	if !strings.HasPrefix(line, "ab") || !strings.HasSuffix(line, "cd") {
		t.Errorf("segments misplaced: %q", line)
	}
	narrow := barLine(2, "abcdef", "xyz")
	if !strings.Contains(narrow, "abcdef xyz") {
		t.Errorf("narrow fallback = %q", narrow)
	}
}

// TestHelpAndRenderStates exercises the /help block and the three status-row
// states (ready / streaming / error) through the public render path.
func TestHelpAndRenderStates(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co", Model: "test/model"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)

	m.runSlash("/help")
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "/reasoning") {
		t.Errorf("/help missing commands: %q", joined)
	}

	if out := m.render(); !strings.Contains(out, "test/model") {
		t.Error("header should carry the model slug")
	}
	m.streaming = true
	if out := m.render(); !strings.Contains(out, "streaming") {
		t.Error("streaming status missing")
	}
	m.streaming = false
	m.statusErr = "boom"
	if out := m.render(); !strings.Contains(out, "boom") {
		t.Error("error status missing")
	}
}
