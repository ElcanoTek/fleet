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

	// Without a note (its write is best-effort) the count falls back to
	// messages strictly after the answer's second: the same-second messages,
	// including the one sent while the card was pending, do not count.
	n, err = s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_missing", resolvedAt)
	if err != nil {
		t.Fatalf("CountUserMessagesAfterResolution: %v", err)
	}
	if n != 0 {
		t.Errorf("count without a note = %d, want 0 (nothing is strictly after the answer's second)", n)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET created_at = $1 WHERE conversation_id = $2 AND content LIKE '%after 2%'`, resolvedAt+1, conv.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_missing", resolvedAt); err != nil || n != 1 {
		t.Errorf("count without a note = %d (err %v), want 1 (a later-second message counts)", n, err)
	}
}
