package store

import (
	"context"
	"testing"
	"time"
)

func enqueueTestEvent(t *testing.T, st *Store, email, role string) AccountEvent {
	t.Helper()
	ev, err := st.EnqueueAccountEvent(context.Background(), AccountEvent{
		Type: AccountEventAccessChanged, Email: email, Enabled: true,
		ChatRole: role, OpsRole: "none", Source: AccountEventSourceAdminUI, Actor: "Boss@X.com",
	})
	if err != nil {
		t.Fatalf("enqueue %s: %v", email, err)
	}
	return ev
}

func TestAccountEventsClaimOnePerEmailInOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a1 := enqueueTestEvent(t, st, "A@x.com", RoleMember)
	a2 := enqueueTestEvent(t, st, "a@x.com", RoleViewer)
	b1 := enqueueTestEvent(t, st, "b@x.com", RoleAdmin)
	if a1.Email != "a@x.com" || a1.Actor != "boss@x.com" || a1.EventID == "" || a1.OccurredAt == 0 {
		t.Fatalf("enqueue did not normalize/fill the row: %+v", a1)
	}
	now := time.Now().Unix()

	claimed, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 || claimed[0].ID != a1.ID || claimed[1].ID != b1.ID {
		t.Fatalf("claimed %+v, want the head of each email (a1, b1)", claimed)
	}
	if claimed[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", claimed[0].Attempts)
	}
	// Leased rows are not claimable again, and a2 waits behind a1.
	again, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Minute)
	if err != nil || len(again) != 0 {
		t.Fatalf("second claim = (%+v, %v), want nothing while leased", again, err)
	}

	// A failed attempt schedules a retry; a2 still waits behind it.
	if err := st.MarkAccountEventFailed(ctx, a1.ID, now, now+30, false, "receiver returned status 503"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAccountEventDelivered(ctx, b1.ID, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ClaimDueAccountEvents(ctx, now+10, 10, time.Minute); len(got) != 0 {
		t.Fatalf("claimed %+v before a1's retry was due", got)
	}
	got, err := st.ClaimDueAccountEvents(ctx, now+30, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != a1.ID || got[0].Attempts != 2 {
		t.Fatalf("retry claim = (%+v, %v), want a1 attempt 2", got, err)
	}
	if err := st.MarkAccountEventDelivered(ctx, a1.ID, now+30); err != nil {
		t.Fatal(err)
	}
	got, err = st.ClaimDueAccountEvents(ctx, now+30, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != a2.ID {
		t.Fatalf("after a1 = (%+v, %v), want a2", got, err)
	}
}

func TestAccountEventsExpiredLeaseIsReclaimed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ev := enqueueTestEvent(t, st, "lease@x.com", RoleMember)
	now := time.Now().Unix()
	if got, err := st.ClaimDueAccountEvents(ctx, now, 1, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("claim = (%+v, %v)", got, err)
	}
	got, err := st.ClaimDueAccountEvents(ctx, now+61, 1, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != ev.ID || got[0].Attempts != 2 {
		t.Fatalf("expired lease claim = (%+v, %v), want the same row, attempt 2", got, err)
	}
}

func TestAccountEventsGiveUpUnblocksNextAndStats(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	first := enqueueTestEvent(t, st, "g@x.com", RoleMember)
	second := enqueueTestEvent(t, st, "g@x.com", RoleViewer)
	now := time.Now().Unix()
	if _, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAccountEventFailed(ctx, first.ID, now, now+3600, true, "receiver returned status 401"); err != nil {
		t.Fatal(err)
	}
	stats, err := st.AccountEventStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Failed != 1 || stats.Delivered != 0 || stats.OldestPendingAt == 0 {
		t.Fatalf("stats = %+v, want 1 pending, 1 failed", stats)
	}
	got, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("after give-up = (%+v, %v), want the next event", got, err)
	}
	if err := st.MarkAccountEventDelivered(ctx, second.ID, now); err != nil {
		t.Fatal(err)
	}

	// Prune keeps rows younger than the cutoffs and removes older ones.
	if n, err := st.PruneAccountEvents(ctx, now-1, now-1); err != nil || n != 0 {
		t.Fatalf("early prune = (%d, %v), want 0", n, err)
	}
	if n, err := st.PruneAccountEvents(ctx, now+1, now+1); err != nil || n != 2 {
		t.Fatalf("prune = (%d, %v), want 2", n, err)
	}
}

func TestAccountAccessIncludesDisabledRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, found, err := st.AccountAccess(ctx, "nobody@x.com"); err != nil || found {
		t.Fatalf("missing account = (%v, %v), want not found", found, err)
	}
	if _, err := st.CreateUser(ctx, "d@x.com", "password-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE users SET enabled = FALSE WHERE email = 'd@x.com'`); err != nil {
		t.Fatal(err)
	}
	a, found, err := st.AccountAccess(ctx, "D@x.com")
	if err != nil || !found || a.Enabled || a.Role != RoleMember {
		t.Fatalf("disabled account = (%+v, %v, %v), want found, disabled, member", a, found, err)
	}
	all, err := st.ListAccountAccess(ctx)
	if err != nil || len(all) != 1 || all[0].Email != "d@x.com" {
		t.Fatalf("list = (%+v, %v)", all, err)
	}
}
