package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// messageIDs returns the message ids of a conversation in insertion order, so a
// test can pick a concrete branch point.
func messageIDs(t *testing.T, s *Store, convID string) []int64 {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT id FROM messages WHERE conversation_id = $1 ORDER BY id ASC`, convID)
	if err != nil {
		t.Fatalf("query message ids: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func seedBranchConv(t *testing.T, s *Store, owner string) *Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := s.CreateConversation(ctx, owner, "Original", "victoria", "openai/gpt-5", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	entries := []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"q1"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"a1"}`)},
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"q2"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"a2"}`)},
	}
	if _, err := s.AppendHistory(ctx, conv.ID, entries); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	return conv
}

// TestBranchConversation_RoundTrip: forking at the 2nd message copies messages
// [1..2] into an independent conversation that records its lineage, leaves the
// parent untouched, and stays independent when the branch is continued (#454).
func TestBranchConversation_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	parent := seedBranchConv(t, s, owner)
	ids := messageIDs(t, s, parent.ID)
	if len(ids) != 4 {
		t.Fatalf("expected 4 seeded messages, got %d", len(ids))
	}

	branchAt := ids[1] // include the first two messages (q1, a1)
	branch, err := s.BranchConversation(ctx, owner, parent.ID, branchAt, "")
	if err != nil {
		t.Fatalf("BranchConversation: %v", err)
	}
	// Lineage recorded; parent settings inherited.
	if branch.ParentConversationID != parent.ID || branch.BranchPointMessageID != branchAt {
		t.Errorf("lineage = (%q,%d), want (%q,%d)", branch.ParentConversationID, branch.BranchPointMessageID, parent.ID, branchAt)
	}
	if branch.Persona != parent.Persona || branch.Model != parent.Model {
		t.Errorf("branch did not inherit persona/model: %+v", branch)
	}
	if branch.ID == parent.ID {
		t.Fatal("branch must be a NEW conversation")
	}

	// The branch has exactly the copied prefix (2 messages); the parent is untouched (4).
	if got := mustHistoryLen(t, s, branch.ID); got != 2 {
		t.Errorf("branch history len = %d, want 2", got)
	}
	if got := mustHistoryLen(t, s, parent.ID); got != 4 {
		t.Errorf("parent history len = %d, want 4 (must be untouched)", got)
	}

	// Lineage round-trips through Get.
	reread, err := s.Get(ctx, owner, branch.ID)
	if err != nil || reread == nil {
		t.Fatalf("Get(branch): %v", err)
	}
	if reread.ParentConversationID != parent.ID || reread.BranchPointMessageID != branchAt {
		t.Errorf("Get lineage = (%q,%d), want (%q,%d)", reread.ParentConversationID, reread.BranchPointMessageID, parent.ID, branchAt)
	}

	// Continuing the branch does NOT affect the parent (messages are copied, not shared).
	if _, err := s.AppendHistory(ctx, branch.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"branch-only"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory(branch): %v", err)
	}
	if got := mustHistoryLen(t, s, parent.ID); got != 4 {
		t.Errorf("after branch-only append, parent history len = %d, want 4 (independent)", got)
	}
	if got := mustHistoryLen(t, s, branch.ID); got != 3 {
		t.Errorf("branch history len = %d, want 3 after continuing", got)
	}

	// A non-branch conversation has empty lineage.
	if parent.ParentConversationID != "" || parent.BranchPointMessageID != 0 {
		t.Errorf("parent should have empty lineage, got (%q,%d)", parent.ParentConversationID, parent.BranchPointMessageID)
	}
}

// TestBranchConversation_Errors: a branch point that names no message in the
// parent — below OR above its id range (#578) — is ErrBranchPointNotFound; a
// parent not owned by the caller is not found. A failed attempt never leaves a
// branch conversation behind.
func TestBranchConversation_Errors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	parent := seedBranchConv(t, s, owner)
	ids := messageIDs(t, s, parent.ID)

	// A branch point id BELOW the parent's lowest message names no message in
	// the parent.
	if _, err := s.BranchConversation(ctx, owner, parent.ID, ids[0]-1, "x"); !errors.Is(err, ErrBranchPointNotFound) {
		t.Errorf("below-range branch point: want ErrBranchPointNotFound, got %v", err)
	}

	// An id ABOVE the parent's highest message names no message either (#578):
	// before the existence check this silently copied the ENTIRE conversation,
	// because id <= $2 matched every row.
	if _, err := s.BranchConversation(ctx, owner, parent.ID, ids[len(ids)-1]+1, "x"); !errors.Is(err, ErrBranchPointNotFound) {
		t.Errorf("above-range branch point: want ErrBranchPointNotFound, got %v", err)
	}

	// An id belonging to ANOTHER conversation (here another user's — the worst
	// case) is rejected too: messages.id is a global BIGSERIAL, so a foreign id
	// is numerically plausible (and, seeded second, above the parent's max).
	other := seedBranchConv(t, s, "bob@example.com")
	otherIDs := messageIDs(t, s, other.ID)
	if _, err := s.BranchConversation(ctx, owner, parent.ID, otherIDs[len(otherIDs)-1], "x"); !errors.Is(err, ErrBranchPointNotFound) {
		t.Errorf("foreign branch point: want ErrBranchPointNotFound, got %v", err)
	}

	// A different user cannot branch alice's conversation.
	if _, err := s.BranchConversation(ctx, "mallory@example.com", parent.ID, 1, "x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("foreign parent: want sql.ErrNoRows, got %v", err)
	}

	// None of the rejected attempts created a conversation.
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE user_email = $1`, owner).Scan(&n); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if n != 1 {
		t.Errorf("conversation count = %d, want 1 (the seeded parent only)", n)
	}
}

// TestBranchConversation_AtomicOnCopyFailure pins the #597 all-or-nothing
// contract: the conversation INSERT and the message copy happen in ONE
// transaction, so when the copy fails nothing is committed — no empty branch
// shell in the sidebar, with or without a compensating delete.
func TestBranchConversation_AtomicOnCopyFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const owner = "alice@example.com"
	parent := seedBranchConv(t, s, owner)
	ids := messageIDs(t, s, parent.ID)

	// Force the copy step to fail: after seeding, reject every further INSERT
	// into messages. The branch's conversation INSERT itself succeeds, so a
	// non-atomic implementation would leave that row behind.
	if _, err := s.db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION fleet_test_fail_message_insert() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced copy failure (test)';
		END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER fleet_test_fail_message_insert BEFORE INSERT ON messages
		FOR EACH ROW EXECUTE FUNCTION fleet_test_fail_message_insert()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		// The shared test database persists across tests — the trigger must not.
		cctx := context.Background()
		_, _ = s.db.ExecContext(cctx, `DROP TRIGGER IF EXISTS fleet_test_fail_message_insert ON messages`)
		_, _ = s.db.ExecContext(cctx, `DROP FUNCTION IF EXISTS fleet_test_fail_message_insert()`)
	})

	if _, err := s.BranchConversation(ctx, owner, parent.ID, ids[1], "torn"); err == nil {
		t.Fatal("expected the forced copy failure to surface")
	}

	// All-or-nothing: no partial branch row was committed.
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE user_email = $1`, owner).Scan(&n); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if n != 1 {
		t.Errorf("conversation count = %d, want 1 (only the parent — no torn branch)", n)
	}
}

func mustHistoryLen(t *testing.T, s *Store, convID string) int {
	t.Helper()
	h, err := s.LoadHistory(context.Background(), convID)
	if err != nil {
		t.Fatalf("LoadHistory(%s): %v", convID, err)
	}
	return len(h)
}
