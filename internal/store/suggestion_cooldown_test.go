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
	t.Parallel()

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
	// AppendHistory stamps the wall clock, which a slow run (-race) can carry
	// into the next second; pin every row to the answer's second, which is the
	// case under test.
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET created_at = $1 WHERE conversation_id = $2`, resolvedAt, conv.ID); err != nil {
		t.Fatal(err)
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

// message content is TEXT holding JSON, and a tool_result row whose JSON holds
// the \u0000 sequence cannot be cast to jsonb. Such a row in the window does
// not fail the count: the query casts only rows without the sequence, so the
// answer's own note is still found, and a note holding the sequence leaves the
// timestamp boundary.
func TestCountUserMessagesAfterResolution_RowWithNULSequence(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, "alice@x.com", "t", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	result := func(content string) agent.HistoryEntry {
		return agent.HistoryEntry{Role: "tool", Type: "tool_result", Content: []byte(content)}
	}
	user := agent.HistoryEntry{Role: "user", Type: "text", Content: []byte(`{"text":"after"}`)}
	resolvedAt := time.Now().Unix()
	for _, batch := range [][]agent.HistoryEntry{
		{result(`{"id":"call_other","text":"a\u0000b"}`)},
		{result(`{"id":"call_1","name":"suggest_advanced_model","text":"dismissed"}`)},
		{result(`{"id":"call_2","text":"x\u0000"}`)},
		{user},
	} {
		if _, err := s.AppendHistory(ctx, conv.ID, batch); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET created_at = $1 WHERE conversation_id = $2`, resolvedAt, conv.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_1", resolvedAt); err != nil || n != 1 {
		t.Fatalf("count after call_1 = %d, %v; want 1", n, err)
	}
	if n, err := s.CountUserMessagesAfterResolution(ctx, conv.ID, "call_2", resolvedAt); err != nil || n != 0 {
		t.Fatalf("count after call_2 (note holds the sequence) = %d, %v; want 0 (the timestamp boundary)", n, err)
	}
}
