package store

import (
	"context"
	"testing"
)

// TestPushSubscriptions_UpsertListDelete covers the lifecycle: insert, list
// (user-scoped, newest first), the endpoint-keyed upsert refreshing keys +
// last_active_at without duplicating rows, and both delete flavors.
func TestPushSubscriptions_UpsertListDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep1", "auth1", "p256dh1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep2", "auth2", "p256dh2"); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	subs, err := s.ListPushSubscriptions(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("list: got %d subscriptions, want 2", len(subs))
	}
	for _, sub := range subs {
		if sub.UserEmail != "u@x.com" || sub.ID == "" || sub.CreatedAt == 0 || sub.LastActiveAt == 0 {
			t.Errorf("row not fully populated: %+v", sub)
		}
	}

	// Re-subscribe on the SAME endpoint: keys refresh in place, no new row.
	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep1", "auth1b", "p256dh1b"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	subs, err = s.ListPushSubscriptions(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("list after re-upsert: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("re-upsert duplicated a row: got %d, want 2", len(subs))
	}
	var found bool
	for _, sub := range subs {
		if sub.Endpoint == "https://push.example/ep1" {
			found = true
			if sub.KeysAuth != "auth1b" || sub.KeysP256dh != "p256dh1b" {
				t.Errorf("keys not refreshed: %+v", sub)
			}
		}
	}
	if !found {
		t.Fatal("re-upserted endpoint missing from list")
	}

	// User scoping: another user sees nothing.
	other, err := s.ListPushSubscriptions(ctx, "other@x.com")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("cross-user list leaked %d rows", len(other))
	}

	// User-scoped delete: the wrong user is a no-op, the owner removes the row.
	if err := s.DeleteUserPushSubscription(ctx, "other@x.com", "https://push.example/ep1"); err != nil {
		t.Fatalf("cross-user delete: %v", err)
	}
	if subs, _ = s.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 2 {
		t.Fatalf("cross-user delete removed a row: got %d, want 2", len(subs))
	}
	if err := s.DeleteUserPushSubscription(ctx, "u@x.com", "https://push.example/ep1"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}

	// Endpoint-only delete (the expired-subscription cleanup path).
	if err := s.DeletePushSubscription(ctx, "https://push.example/ep2"); err != nil {
		t.Fatalf("endpoint delete: %v", err)
	}
	if subs, _ = s.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 0 {
		t.Fatalf("expected no rows left, got %d", len(subs))
	}

	// Idempotent: deleting an absent endpoint is not an error.
	if err := s.DeletePushSubscription(ctx, "https://push.example/gone"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

// TestPushSubscriptions_UpsertValidation rejects rows missing any required
// field — the HTTP layer relies on the store as the final gate.
func TestPushSubscriptions_UpsertValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cases := []struct{ user, endpoint, auth, p256dh string }{
		{"", "https://push.example/ep", "a", "p"},
		{"u@x.com", "", "a", "p"},
		{"u@x.com", "https://push.example/ep", "", "p"},
		{"u@x.com", "https://push.example/ep", "a", ""},
	}
	for i, c := range cases {
		if err := s.UpsertPushSubscription(ctx, c.user, c.endpoint, c.auth, c.p256dh); err == nil {
			t.Errorf("case %d: want validation error, got nil", i)
		}
	}
}
