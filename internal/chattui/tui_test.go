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
	m.history = append(m.history, styleUserLabel.Render("you")+"\nhello")
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
