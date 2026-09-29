package store

import (
	"context"
	"strings"
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

func TestAccountEventsReleaseLeasesAndLastErrorByFailureTime(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	older := enqueueTestEvent(t, st, "old@x.com", RoleMember)
	newer := enqueueTestEvent(t, st, "new@x.com", RoleMember)
	now := time.Now().Unix()
	claimed, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Hour)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim = (%+v, %v)", claimed, err)
	}
	// A stopped deliverer hands both back: claimable again at once, with the
	// cut-off attempt not counted.
	if err := st.ReleaseAccountEventLeases(ctx, []int64{older.ID, newer.ID}); err != nil {
		t.Fatal(err)
	}
	again, err := st.ClaimDueAccountEvents(ctx, now, 10, time.Hour)
	if err != nil || len(again) != 2 || again[0].Attempts != 1 {
		t.Fatalf("reclaim after release = (%+v, %v), want both rows back at attempt 1", again, err)
	}
	// The newer-id event gave up long ago; the older-id one failed just now.
	// status must report the fresh failure.
	if err := st.MarkAccountEventFailed(ctx, newer.ID, now-86400, now, true, "receiver returned status 500"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkAccountEventFailed(ctx, older.ID, now, now+60, false, "receiver returned status 401"); err != nil {
		t.Fatal(err)
	}
	stats, err := st.AccountEventStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LastError != "receiver returned status 401" {
		t.Fatalf("last error = %q, want the most recent failure", stats.LastError)
	}
}

func TestDeletedAccountEmailsAndAdoptFleetAccessChange(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, email := range []string{"kept@x.com", "idp@x.com"} {
		if _, err := st.CreateUser(ctx, email, "pw-123456789"); err != nil {
			t.Fatal(err)
		}
	}
	state := ExternalAccessState{
		Issuer: "https://auth.example.com", Subject: "s1", Email: "idp@x.com", Version: 3, Allowed: true,
		EventID: "e3", ChatRole: RoleMember, OpsRole: "readonly", IssuedAt: 1_000,
	}
	if _, _, err := st.ApplyExternalAccess(ctx, state); err != nil {
		t.Fatal(err)
	}
	// A Fleet-side role change becomes the provider row's baseline; the version
	// stays the provider's.
	token, err := st.ProviderStateToken(ctx, "idp@x.com")
	if err != nil || token == "" {
		t.Fatalf("token = (%q, %v)", token, err)
	}
	if ok, err := st.AdoptFleetAccessChange(ctx, "IDP@x.com", token, true, RoleViewer, "client"); err != nil || !ok {
		t.Fatalf("adopt = (%v, %v)", ok, err)
	}
	got, _, err := st.ExternalAccessState(ctx, state.Issuer, state.Subject)
	if err != nil || got.ChatRole != RoleViewer || got.OpsRole != "client" || got.Version != 3 || !got.Allowed {
		t.Fatalf("state after adopt = (%+v, %v)", got, err)
	}
	// A provider push that lands while a Fleet change is in flight is the more
	// recent change: the adoption, guarded by the token read before it, skips.
	token, _ = st.ProviderStateToken(ctx, "idp@x.com")
	newer := state
	newer.Version, newer.EventID, newer.ChatRole, newer.OpsRole = 4, "e4", RoleAdmin, "admin"
	if _, _, err := st.ApplyExternalAccess(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AdoptFleetAccessChange(ctx, "idp@x.com", token, true, RoleViewer, "readonly"); err != nil || ok {
		t.Fatalf("adopt over a newer push = (%v, %v), want skipped", ok, err)
	}
	got, _, _ = st.ExternalAccessState(ctx, state.Issuer, state.Subject)
	if got.Version != 4 || got.ChatRole != RoleAdmin || got.OpsRole != "admin" {
		t.Fatalf("state after a skipped adopt = %+v, want the provider's v4 roles", got)
	}
	// Outbox history: one account gone after an access event, one whose latest
	// event is already a deletion, one that still exists.
	enqueueTestEvent(t, st, "lost-delete@x.com", RoleMember)
	enqueueTestEvent(t, st, "reported@x.com", RoleMember)
	if _, err := st.EnqueueAccountEvent(ctx, AccountEvent{Type: AccountEventDeleted, Email: "reported@x.com", Source: AccountEventSourceCLI}); err != nil {
		t.Fatal(err)
	}
	enqueueTestEvent(t, st, "kept@x.com", RoleMember)
	if err := st.DeleteUser(ctx, "idp@x.com"); err != nil {
		t.Fatal(err)
	}
	token, _ = st.ProviderStateToken(ctx, "idp@x.com")
	if ok, err := st.AdoptFleetAccessChange(ctx, "idp@x.com", token, false, "", ""); err != nil || !ok {
		t.Fatalf("adopt deletion = (%v, %v)", ok, err)
	}
	got, _, _ = st.ExternalAccessState(ctx, state.Issuer, state.Subject)
	if got.Allowed || got.ChatRole != RoleAdmin {
		t.Fatalf("state after a Fleet deletion = %+v, want not allowed with its roles kept", got)
	}
	gone, err := st.DeletedAccountEmails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gone, ",") != "idp@x.com,lost-delete@x.com" {
		t.Fatalf("deleted emails = %v, want the provider-known and the history-known gone accounts", gone)
	}
}
