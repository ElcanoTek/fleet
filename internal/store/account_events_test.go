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

func provisionedTestAccount(t *testing.T, st *Store) ExternalAccessState {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "idp@x.com", "pw-123456789"); err != nil {
		t.Fatal(err)
	}
	state := ExternalAccessState{
		Issuer: "https://auth.example.com", Subject: "s1", Email: "idp@x.com", Version: 3, Allowed: true,
		EventID: "e3", ChatRole: RoleMember, OpsRole: "readonly", IssuedAt: 1_000,
	}
	if _, _, err := st.ApplyExternalAccess(ctx, state); err != nil {
		t.Fatal(err)
	}
	return state
}

// adopt reads a fresh token and adopts under it.
func adopt(t *testing.T, st *Store, exists, enabled bool, chatRole, opsRole string) {
	t.Helper()
	token, err := st.ProviderStateToken(context.Background(), "idp@x.com")
	if err != nil || token == "" {
		t.Fatalf("token = (%q, %v)", token, err)
	}
	if ok, err := st.AdoptFleetAccessChange(context.Background(), "IDP@x.com", token, exists, enabled, chatRole, opsRole); err != nil || !ok {
		t.Fatalf("adopt = (%v, %v)", ok, err)
	}
}

func providerState(t *testing.T, st *Store, s ExternalAccessState) ExternalAccessState {
	t.Helper()
	got, _, err := st.ExternalAccessState(context.Background(), s.Issuer, s.Subject)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAdoptFleetAccessChange: a Fleet-side change becomes the provider row's
// baseline (roles and allowed; never the version), a deletion marks it not
// allowed, and a re-creation allows it again.
func TestAdoptFleetAccessChange(t *testing.T) {
	st := newTestStore(t)
	state := provisionedTestAccount(t, st)
	adopt(t, st, true, true, RoleViewer, "client")
	if got := providerState(t, st, state); got.ChatRole != RoleViewer || got.OpsRole != "client" || got.Version != 3 || !got.Allowed {
		t.Fatalf("after adopt = %+v", got)
	}
	adopt(t, st, false, false, "", "")
	if got := providerState(t, st, state); got.Allowed || got.ChatRole != RoleViewer {
		t.Fatalf("after a Fleet deletion = %+v, want not allowed with its roles kept", got)
	}
	adopt(t, st, true, true, RoleMember, "readonly")
	if got := providerState(t, st, state); !got.Allowed || got.OpsRole != "readonly" {
		t.Fatalf("after a re-creation = %+v, want allowed again", got)
	}
}

// TestAdoptFleetAccessChangeSkipsWhenTheRowMoved: the token read before a
// Fleet change guards its adoption against a provider push that landed
// meanwhile; another Fleet change's adoption does not move it (those are
// ordered by the per-account lock, and the later one must win).
func TestAdoptFleetAccessChangeSkipsWhenTheRowMoved(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	state := provisionedTestAccount(t, st)

	token, _ := st.ProviderStateToken(ctx, "idp@x.com")
	newer := state
	newer.Version, newer.EventID, newer.ChatRole, newer.OpsRole = 4, "e4", RoleAdmin, "admin"
	if _, _, err := st.ApplyExternalAccess(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AdoptFleetAccessChange(ctx, "idp@x.com", token, true, true, RoleViewer, "readonly"); err != nil || ok {
		t.Fatalf("adopt over a newer push = (%v, %v), want skipped", ok, err)
	}
	if got := providerState(t, st, state); got.Version != 4 || got.OpsRole != "admin" {
		t.Fatalf("after a skipped adopt = %+v, want the provider's v4", got)
	}

	token, _ = st.ProviderStateToken(ctx, "idp@x.com")
	if ok, err := st.AdoptFleetAccessChange(ctx, "idp@x.com", token, true, true, RoleMember, "client"); err != nil || !ok {
		t.Fatalf("first overlapping adopt = (%v, %v)", ok, err)
	}
	if ok, err := st.AdoptFleetAccessChange(ctx, "idp@x.com", token, true, true, RoleViewer, "readonly"); err != nil || !ok {
		t.Fatalf("later Fleet adopt = (%v, %v), want it to supersede the earlier one", ok, err)
	}
	if got := providerState(t, st, state); got.ChatRole != RoleViewer || got.OpsRole != "readonly" {
		t.Fatalf("after two Fleet adopts = %+v, want the later one", got)
	}
}

// TestDeletedAccountEmails: gone accounts known from provider rows or outbox
// history, including a deletion the receiver rejected; not one whose deletion
// is already reported, nor one that still exists.
func TestDeletedAccountEmails(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	provisionedTestAccount(t, st)
	if _, err := st.CreateUser(ctx, "kept@x.com", "pw-123456789"); err != nil {
		t.Fatal(err)
	}
	enqueueTestEvent(t, st, "lost-delete@x.com", RoleMember)
	enqueueTestEvent(t, st, "reported@x.com", RoleMember)
	if _, err := st.EnqueueAccountEvent(ctx, AccountEvent{Type: AccountEventDeleted, Email: "reported@x.com", Source: AccountEventSourceCLI}); err != nil {
		t.Fatal(err)
	}
	enqueueTestEvent(t, st, "kept@x.com", RoleMember)
	rejected, err := st.EnqueueAccountEvent(ctx, AccountEvent{Type: AccountEventDeleted, Email: "rejected@x.com", Source: AccountEventSourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := st.MarkAccountEventFailed(ctx, rejected.ID, now, now, true, "receiver rejected the event: status 400"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, "idp@x.com"); err != nil {
		t.Fatal(err)
	}
	gone, err := st.DeletedAccountEmails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gone, ",") != "idp@x.com,lost-delete@x.com,rejected@x.com" {
		t.Fatalf("deleted emails = %v", gone)
	}
}

// TestInAccountEventsTxSerializesOneAccount: a second transaction on the same
// account waits for the first to commit; another account's does not; and the
// work runs on the transaction's own connection, so a one-connection pool
// cannot starve it.
func TestInAccountEventsTxSerializesOneAccount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = st.InAccountEventsTx(ctx, "Lock@x.com", func(tx *AccountEventsTx) error {
			close(held)
			<-release
			_, err := tx.EnqueueAccountEvent(ctx, AccountEvent{Type: AccountEventAccessChanged, Email: "lock@x.com",
				Enabled: true, ChatRole: RoleMember, OpsRole: "none", Source: AccountEventSourceCLI})
			return err
		})
	}()
	<-held
	if err := st.InAccountEventsTx(ctx, "other@x.com", func(*AccountEventsTx) error { return nil }); err != nil {
		t.Fatalf("another account's tx: %v", err)
	}
	got := make(chan struct{})
	go func() {
		_ = st.InAccountEventsTx(ctx, "lock@x.com", func(tx *AccountEventsTx) error {
			_, _, err := tx.AccountAccess(ctx, "lock@x.com")
			return err
		})
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second transaction took the account's lock while it was held")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not handed on after commit")
	}
	stats, err := st.AccountEventStats(ctx)
	if err != nil || stats.Pending != 1 {
		t.Fatalf("stats = (%+v, %v), want the first transaction's event committed", stats, err)
	}
}

// TestInAccountEventsTxRunsOnAOneConnectionPool: FLEET_CHAT_DB_MAX_CONNS=1 is
// a valid setting, and the whole locked commit — read, enqueue, adoption —
// must fit on that one connection instead of waiting on the pool it holds.
func TestInAccountEventsTxRunsOnAOneConnectionPool(t *testing.T) {
	newTestStore(t) // truncates; skips without a DSN
	cfg := DefaultPoolConfig()
	cfg.MaxOpenConns, cfg.MaxIdleConns = 1, 1
	st, err := Open(testDSN(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provisionedTestAccount(t, st)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := st.ProviderStateToken(ctx, "idp@x.com")
	if err != nil {
		t.Fatal(err)
	}
	err = st.InAccountEventsTx(ctx, "idp@x.com", func(tx *AccountEventsTx) error {
		if _, _, err := tx.AccountAccess(ctx, "idp@x.com"); err != nil {
			return err
		}
		if _, err := tx.EnqueueAccountEvent(ctx, AccountEvent{Type: AccountEventAccessChanged, Email: "idp@x.com",
			Enabled: true, ChatRole: RoleMember, OpsRole: "client", Source: AccountEventSourceAdminUI}); err != nil {
			return err
		}
		_, err := tx.AdoptFleetAccessChange(ctx, "idp@x.com", token, true, true, RoleMember, "client")
		return err
	})
	if err != nil {
		t.Fatalf("locked commit on a one-connection pool: %v", err)
	}
}
