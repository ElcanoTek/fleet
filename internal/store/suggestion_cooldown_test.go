package store

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// The suggestion cooldown counts user messages after the card's answer in
// message order, not by timestamp: a message sent while the card sat pending
// does not count even when the answer lands in the same second.
func TestCountUserMessagesAfterResolution_SameSecond(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "alice@x.com"
	conv, err := s.CreateConversation(ctx, owner, "t", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	user := func(text string) agent.HistoryEntry {
		return agent.HistoryEntry{Role: "user", Type: "text", Content: []byte(`{"text":"` + text + `"}`)}
	}
	note := agent.HistoryEntry{Role: "tool", Type: "tool_result", Content: []byte(`{"id":"call_1","name":"suggest_advanced_model","text":"dismissed"}`)}
	resolvedAt := time.Now().Unix()

	// Sent while the card was pending, then the answer, then two after it.
	for _, batch := range [][]agent.HistoryEntry{{user("before")}, {note}, {user("after 1")}, {user("after 2")}} {
		if _, err := s.AppendHistory(ctx, conv.ID, batch); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}
	n, err := s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_1", resolvedAt)
	if err != nil {
		t.Fatalf("CountUserMessagesAfterResolution: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (the message sent before the answer must not count)", n)
	}

	// Without a note the timestamp bound alone applies.
	n, err = s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_missing", resolvedAt+3600)
	if err != nil {
		t.Fatalf("CountUserMessagesAfterResolution: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 (nothing is after a later resolution)", n)
	}
}
