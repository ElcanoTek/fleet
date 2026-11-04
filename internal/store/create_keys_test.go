package store

import (
	"context"
	"testing"
)

// TestCreateKeys pins the idempotency-key rules: the first bind wins (a
// concurrent create with the same key gets the winner back), lookups are per
// user, Forget clears a key, and the account's keys go with the account.
func TestCreateKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const me = "keys@elcanotek.com"
	if _, err := s.CreateUser(ctx, me, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ConversationForCreateKey(ctx, me, "k"); err != nil || id != "" {
		t.Fatalf("unknown key = %q, %v", id, err)
	}
	if bound, err := s.RecordCreateKey(ctx, me, "k", "conv-a"); err != nil || bound != "conv-a" {
		t.Fatalf("first bind = %q, %v", bound, err)
	}
	if bound, err := s.RecordCreateKey(ctx, me, "k", "conv-b"); err != nil || bound != "conv-a" {
		t.Fatalf("second bind = %q, %v; want the first (conv-a)", bound, err)
	}
	if id, _ := s.ConversationForCreateKey(ctx, me, "k"); id != "conv-a" {
		t.Errorf("lookup = %q, want conv-a", id)
	}
	if id, _ := s.ConversationForCreateKey(ctx, "someone@elcanotek.com", "k"); id != "" {
		t.Errorf("another user's lookup = %q, want none", id)
	}
	if err := s.ForgetCreateKey(ctx, me, "k"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.ConversationForCreateKey(ctx, me, "k"); id != "" {
		t.Errorf("after Forget = %q, want none", id)
	}
	if _, err := s.RecordCreateKey(ctx, me, "k2", "conv-c"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, me); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_create_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("create keys after DeleteUser = %d, want 0", n)
	}
}
