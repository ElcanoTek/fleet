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

	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep1", "fake-auth-one", "fake-p256dh-one"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep2", "fake-auth-two", "fake-p256dh-two"); err != nil {
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
	if err := s.UpsertPushSubscription(ctx, "u@x.com", "https://push.example/ep1", "fake-auth-refreshed", "fake-p256dh-refreshed"); err != nil {
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
			if sub.KeysAuth != "fake-auth-refreshed" || sub.KeysP256dh != "fake-p256dh-refreshed" {
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

// TestPushSubscriptions_HasUser answers for the owner only, follows a
// re-subscribe that hands the endpoint to another account, and is
// case-insensitive on the email like every other push query.
func TestPushSubscriptions_HasUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const ep = "https://push.example/held"

	held := func(user string) bool {
		t.Helper()
		ok, err := s.HasUserPushSubscription(ctx, user, ep)
		if err != nil {
			t.Fatalf("has %s: %v", user, err)
		}
		return ok
	}
	if held("u@x.com") {
		t.Fatal("held before any upsert")
	}
	if err := s.UpsertPushSubscription(ctx, "u@x.com", ep, "fake-auth", "fake-p256dh"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !held("u@x.com") || !held("U@X.com") {
		t.Error("owner does not hold its own row")
	}
	if held("other@x.com") {
		t.Error("another account holds the owner's row")
	}
	// The same browser re-subscribed while another account was signed in.
	if err := s.UpsertPushSubscription(ctx, "other@x.com", ep, "fake-auth", "fake-p256dh"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if held("u@x.com") || !held("other@x.com") {
		t.Error("ownership did not follow the newest claim")
	}
}

// TestPushSubscriptions_Renew moves the owner's row to the browser's new
// subscription after a VAPID key rotation, refuses any other account, and is
// idempotent when a second tab already renewed.
func TestPushSubscriptions_Renew(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const oldEP, newEP = "https://push.example/old-key", "https://push.example/new-key"
	if err := s.UpsertPushSubscription(ctx, "u@x.com", oldEP, "fake-auth-old", "fake-p256dh-old"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Another account cannot take the row over.
	renewed, err := s.RenewPushSubscription(ctx, "other@x.com", oldEP, newEP, "fake-auth-new", "fake-p256dh-new")
	if err != nil || renewed {
		t.Fatalf("other account renew = %v, %v; want false, nil", renewed, err)
	}
	if subs, _ := s.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 1 || subs[0].Endpoint != oldEP {
		t.Fatalf("a refused renewal changed the row: %+v", subs)
	}

	// The owner's renewal moves the row, keys and all.
	renewed, err = s.RenewPushSubscription(ctx, "U@x.com", oldEP, newEP, "fake-auth-new", "fake-p256dh-new")
	if err != nil || !renewed {
		t.Fatalf("owner renew = %v, %v; want true, nil", renewed, err)
	}
	subs, _ := s.ListPushSubscriptions(ctx, "u@x.com")
	if len(subs) != 1 || subs[0].Endpoint != newEP || subs[0].KeysAuth != "fake-auth-new" || subs[0].KeysP256dh != "fake-p256dh-new" {
		t.Fatalf("row after renew: %+v", subs)
	}

	// A second tab renewing the same move finds it done.
	if renewed, err = s.RenewPushSubscription(ctx, "u@x.com", oldEP, newEP, "fake-auth-new", "fake-p256dh-new"); err != nil || !renewed {
		t.Fatalf("repeated renew = %v, %v; want true, nil", renewed, err)
	}
	if renewed, err = s.RenewPushSubscription(ctx, "other@x.com", oldEP, newEP, "fake-auth-new", "fake-p256dh-new"); err != nil || renewed {
		t.Fatalf("other account after the move = %v, %v; want false, nil", renewed, err)
	}

	if _, err := s.RenewPushSubscription(ctx, "u@x.com", "", newEP, "a", "p"); err == nil {
		t.Error("renew without the old endpoint: want an error")
	}
}

// TestPushSubscriptions_RenewOntoAStoredEndpoint: when the new endpoint is
// already stored for the same user (another tab turned notifications on with
// it) while the old row still exists, the UNIQUE endpoint makes the move
// impossible. The renewal still succeeds by retiring the old row; for another
// account's row it changes nothing.
func TestPushSubscriptions_RenewOntoAStoredEndpoint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const oldEP, newEP, othersEP = "https://push.example/r-old", "https://push.example/r-new", "https://push.example/r-others"
	for _, row := range [][2]string{{"u@x.com", oldEP}, {"u@x.com", newEP}, {"other@x.com", othersEP}} {
		if err := s.UpsertPushSubscription(ctx, row[0], row[1], "fake-auth", "fake-p256dh"); err != nil {
			t.Fatalf("upsert %v: %v", row, err)
		}
	}

	renewed, err := s.RenewPushSubscription(ctx, "u@x.com", oldEP, newEP, "fake-auth-new", "fake-p256dh-new")
	if err != nil || !renewed {
		t.Fatalf("renew onto own stored endpoint = %v, %v; want true, nil", renewed, err)
	}
	if subs, _ := s.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 1 || subs[0].Endpoint != newEP {
		t.Fatalf("rows after renew: %+v, want only the new endpoint", subs)
	}

	if err := s.UpsertPushSubscription(ctx, "u@x.com", oldEP, "fake-auth", "fake-p256dh"); err != nil {
		t.Fatalf("re-upsert old: %v", err)
	}
	renewed, err = s.RenewPushSubscription(ctx, "u@x.com", oldEP, othersEP, "fake-auth-new", "fake-p256dh-new")
	if err != nil || renewed {
		t.Fatalf("renew onto another account's endpoint = %v, %v; want false, nil", renewed, err)
	}
	if subs, _ := s.ListPushSubscriptions(ctx, "other@x.com"); len(subs) != 1 || subs[0].Endpoint != othersEP {
		t.Fatalf("another account's row changed: %+v", subs)
	}
	if subs, _ := s.ListPushSubscriptions(ctx, "u@x.com"); len(subs) != 2 {
		t.Fatalf("a refused renewal removed the caller's rows: %+v", subs)
	}
}
