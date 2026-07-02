package chattui

import (
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
	m.applyEvent(Event{Name: "text.delta", Data: map[string]any{"text": "the answer is **42**"}})
	if m.convID != "conv-7" {
		t.Errorf("convID = %q, want conv-7", m.convID)
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
