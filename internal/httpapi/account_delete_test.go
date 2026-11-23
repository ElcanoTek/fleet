package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/chattui"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Deleting an account from Settings → Admin → Users deletes the user API keys
// that act as it: the key stops authenticating at once, and a later account
// under the same email does not get it.
func TestAdminDeleteRemovesTheAccountsUserKeys(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	mgr, userKey, taskKey := edgeKeys(t) // userKey acts as u@x.com
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey}).CheckIdentity(context.Background()); err != nil {
		t.Fatalf("user key before the delete: %v", err)
	}
	if w := do(t, s.Routes(), http.MethodDelete, "/admin/users/u@x.com", nil, "boss@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d (body %s)", w.Code, w.Body.String())
	}
	for _, k := range mgr.ListKeys() {
		if strings.EqualFold(k.UserEmail, "u@x.com") {
			t.Errorf("user key %s for the deleted account is still in the key store", k.KeyID)
		}
	}
	seedUser(t, s.concreteStore(t), "u@x.com")
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey}).CheckIdentity(context.Background()); err == nil {
		t.Fatal("the deleted account's user key authenticates the account created after it")
	}
	if ok, _, reason := mgr.ValidateKey(context.Background(), taskKey, nil, nil, nil); !ok {
		t.Fatalf("a key that names no account was removed with the account: %s", reason)
	}
}

// A user API key acts only as the account it is bound to: a key left behind
// by an account deleted outside this process (no key cleanup) does not
// authenticate a later account under the same email, and a key minted for
// the new account does.
func TestUserKeyDoesNotActAsALaterAccount(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	mgr, userKey, _ := edgeKeys(t)
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	ctx := context.Background()
	st := s.concreteStore(t)

	// edgeKeys mints the key without an account (as keys were before they
	// named one): its first use binds it to u@x.com's account.
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey}).CheckIdentity(ctx); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := st.DeleteUser(ctx, "u@x.com"); err != nil {
		t.Fatal(err)
	}
	seedUser(t, st, "u@x.com")
	err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey}).CheckIdentity(ctx)
	if err == nil || !strings.Contains(err.Error(), "minted for an earlier account") {
		t.Fatalf("a key bound to the deleted account: %v, want refused as key_predates_account", err)
	}

	u, err := st.GetUser(ctx, "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	_, fresh, err := mgr.CreateBoundUserKey(ctx, "cli-2", "u@x.com", u.ID, 0, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: fresh}).CheckIdentity(ctx); err != nil {
		t.Fatalf("a key minted for the new account: %v", err)
	}
}

// A key minted before keys named their account, whose email has no account
// on its first use, is marked as acting as no one: an account created under
// the email afterwards does not make it work.
func TestUnboundUserKeyWithoutAccountActsAsNoOne(t *testing.T) {
	t.Parallel()
	s := memberFixture(t)
	mgr, _, _ := edgeKeys(t)
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	ctx := context.Background()
	key, ghostKey, err := mgr.CreateUserKey(ctx, "ghost", "ghost@x.com", 0, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, when := range []string{"with no account", "after an account is created under the email"} {
		if i == 1 {
			seedUser(t, s.concreteStore(t), "ghost@x.com")
		}
		err := chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: ghostKey}).CheckIdentity(ctx)
		if err == nil || !strings.Contains(err.Error(), "minted for an earlier account") {
			t.Fatalf("%s: %v, want refused as key_predates_account", when, err)
		}
	}
	if k := mgr.GetKey(key.KeyID); !k.AccountMissing || k.AccountID != "" {
		t.Fatalf("key = %+v, want marked as having no account", k)
	}
}

// A key minted before keys named their account is admitted only once its
// binding is written: while another process keeps the key store locked, its
// first use is answered 503 with Retry-After and reaches no handler; once the
// lock is free the next request binds it and is admitted.
func TestUnboundUserKeyWaitsForItsBinding(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	mgr, err := apikeys.NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	key, raw, err := mgr.CreateUserKey(context.Background(), "cli", "u@x.com", 0, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetFileLockWait(50 * time.Millisecond)
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	// me returns GET /me's status, Retry-After and "error" code.
	me := func() (int, string, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", raw)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, resp.Header.Get("Retry-After"), body.Error
	}

	holder, err := os.Open(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if status, retry, code := me(); status != http.StatusServiceUnavailable || retry == "" || code != "key_store_busy" {
		t.Fatalf("first use with the key store locked: status %d, Retry-After %q, error %q; want 503 key_store_busy with Retry-After",
			status, retry, code)
	}
	if k := mgr.GetKey(key.KeyID); k.AccountID != "" || k.AccountMissing {
		t.Fatalf("key = %+v, want still unbound", k)
	}

	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := me(); status != http.StatusOK {
		t.Fatalf("first use with the key store free: status %d, want 200", status)
	}
	u, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if k := mgr.GetKey(key.KeyID); k.AccountID != u.ID {
		t.Fatalf("key = %+v, want bound to %s", k, u.ID)
	}
}

// Account deletion removes the upload and private workspace trees by the
// paths accountdelete computes; they must be the paths this package writes.
func TestAccountDeletionPathsMatchTheServers(t *testing.T) {
	t.Parallel()
	for _, email := range []string{"u@x.com", " Mixed.Case@X.com "} {
		if got, want := accountdelete.UploadsDir("/att", email), userUploadsRoot("/att", email); got != want {
			t.Errorf("uploads of %q: accountdelete removes %q, the server writes %q", email, got, want)
		}
		if got, want := accountdelete.Digest(email), participantDigest(email); got != want {
			t.Errorf("private workspace digest of %q: accountdelete removes %q, the server writes %q", email, got, want)
		}
	}
	s := &Server{cfg: &config.Config{EmailAttachmentDir: "/att"}}
	root, err := s.privateWorkspaceStashRoot()
	if err != nil || root != accountdelete.PrivateWorkspaceRoot("/att") {
		t.Errorf("private workspace root: server %q (err %v), accountdelete %q", root, err, accountdelete.PrivateWorkspaceRoot("/att"))
	}
}

// The account gate: closing an email cancels its requests in flight and
// refuses new ones until reopened; wait returns once they have ended.
func TestAccountGate(t *testing.T) {
	t.Parallel()
	var g accountGate
	ctx, leave, ok := g.enter(context.Background(), "U@x.com")
	if !ok {
		t.Fatal("an open gate refused a request")
	}
	other, leaveOther, _ := g.enter(context.Background(), "other@x.com")
	defer leaveOther()
	reopen := g.close(" u@x.com ")
	if ctx.Err() == nil {
		t.Fatal("closing did not cancel the request in flight")
	}
	if other.Err() != nil {
		t.Fatal("closing one email cancelled another's request")
	}
	if _, _, ok := g.enter(context.Background(), "u@x.com"); ok {
		t.Fatal("a closed gate admitted a request")
	}
	if drained, err := g.wait(context.Background(), "u@x.com", 50*time.Millisecond); drained || err != nil {
		t.Fatalf("wait with a request still in flight = %v, %v; want not drained", drained, err)
	}
	// Two deletions of the account waiting at once are both woken when the
	// last request ends, long before their deadline.
	woke := make(chan time.Duration, 2)
	for i := 0; i < 2; i++ {
		go func() {
			start := time.Now()
			if drained, err := g.wait(context.Background(), "u@x.com", 10*time.Second); !drained || err != nil {
				t.Errorf("wait = %v, %v", drained, err)
			}
			woke <- time.Since(start)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	leave()
	for i := 0; i < 2; i++ {
		if took := <-woke; took > 5*time.Second {
			t.Errorf("a waiter woke only after %v", took)
		}
	}
	// A wait whose context ends returns at once with its error.
	_, leave2, _ := g.enter(context.Background(), "v@x.com")
	defer leave2()
	ctx2, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := g.wait(ctx2, "v@x.com", 10*time.Second); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("a cancelled wait: err %v after %v", err, time.Since(start))
	}
	reopen()
	reopen() // idempotent
	if _, l, ok := g.enter(context.Background(), "u@x.com"); !ok {
		t.Fatal("a reopened gate refused a request")
	} else {
		l()
	}
}

// While an account is being deleted its requests are refused as not a
// member, through the membership check.
func TestMembershipRefusesAnAccountBeingDeleted(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	reopen := s.accounts.close("u@x.com")
	if w := do(t, s.Routes(), http.MethodGet, "/auth/membership", nil, "u@x.com"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_a_member") {
		t.Fatalf("during the deletion: status %d body %s", w.Code, w.Body.String())
	}
	reopen()
	if w := do(t, s.Routes(), http.MethodGet, "/auth/membership", nil, "u@x.com"); w.Code != http.StatusOK {
		t.Fatalf("after: status %d body %s", w.Code, w.Body.String())
	}
}

// A deletion that completes but leaves a step answers 200 with the warnings
// (not an error status: the account is gone, a retry would only meet 404).
func TestAdminDeleteReportsStepsLeft(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	s.apiKeys = nil // no key store reachable: the key step cannot complete
	w := do(t, s.Routes(), http.MethodDelete, "/admin/users/u@x.com", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200 with warnings", w.Code, w.Body.String())
	}
	var body struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Warnings) != 1 || !strings.Contains(body.Warnings[0], "API-key store") {
		t.Fatalf("body %s (err %v), want one warning about the key store", w.Body.String(), err)
	}
	if _, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com"); err == nil {
		t.Fatal("the account survived")
	}
	// A complete deletion is still 204.
	s.apiKeys = newTestKeyStore(t)
	seedUser(t, s.concreteStore(t), "v@x.com")
	if w := do(t, s.Routes(), http.MethodDelete, "/admin/users/v@x.com", nil, "boss@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("complete deletion: status %d body %s", w.Code, w.Body.String())
	}
}

// A refused deletion stops nothing: the account's requests keep being served.
func TestAdminDeleteRefusalLeavesTheAccountRunning(t *testing.T) {
	t.Parallel()
	f := newTeamHTTPFixture(t)
	req := httptest.NewRequest(http.MethodDelete, "/admin/users/alice%40x.com", nil)
	req = req.WithContext(asUser(req.Context(), "root@x.com"))
	w := httptest.NewRecorder()
	f.srv.handleAdminUserDelete(w, req, "alice@x.com")
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d body %s, want the 409 refusal", w.Code, w.Body.String())
	}
	if _, leave, ok := f.srv.accounts.enter(context.Background(), "alice@x.com"); !ok {
		t.Fatal("a refused deletion left the account's requests refused")
	} else {
		leave()
	}
}

// The maintenance pass removes what an address with no account left — its
// upload tree, its private workspace trees in multiplayer chats, its in-memory
// approvals, its rows — and keeps a live account's.
func TestMaintenanceSweepsDeletedAccounts(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "live@x.com")
	s.sessionApprovals = NewSessionApprovalRegistry()
	att := t.TempDir()
	s.cfg.EmailAttachmentDir = att
	ctx := context.Background()
	var dirs []string
	for _, e := range []string{"live@x.com", "gone@x.com"} {
		for _, d := range []string{
			filepath.Join(accountdelete.UploadsDir(att, e), "tok"),
			filepath.Join(accountdelete.PrivateWorkspaceRoot(att), "conv-1", "participants", accountdelete.Digest(e)),
		} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
			dirs = append(dirs, d)
		}
		s.sessionApprovals.RegisterForUser("conv-1", e, "run_python", SessionApprovalPolicy{Mode: "approve"})
	}
	// A row an account deleted before deletion reached its table left behind
	// (no account writes one for an address without an account any more).
	if _, err := s.concreteStore(t).Identity().DB().ExecContext(ctx,
		`INSERT INTO chat.push_subscriptions (id, user_email, endpoint, keys_auth, keys_p256dh, created_at, last_active_at)
		 VALUES ('left-behind', 'gone@x.com', 'https://push.example/gone', 'a', 'k', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	s.sweepDeletedAccounts(ctx)
	for i, d := range dirs {
		_, err := os.Stat(d)
		live := i < 2
		if live && err != nil {
			t.Errorf("a live account's %s was removed: %v", d, err)
		}
		if !live && !os.IsNotExist(err) {
			t.Errorf("%s of an address with no account survived (err %v)", d, err)
		}
	}
	if _, ok := s.sessionApprovals.MatchForUser("conv-1", "live@x.com", "run_python", "{}"); !ok {
		t.Error("the live account's approval was dropped")
	}
	if _, ok := s.sessionApprovals.MatchForUser("conv-1", "gone@x.com", "run_python", "{}"); ok {
		t.Error("the approval of an address with no account survived")
	}
	if subs, _ := s.concreteStore(t).ListPushSubscriptions(ctx, "gone@x.com"); len(subs) != 0 {
		t.Errorf("%d push subscriptions of an address with no account survived", len(subs))
	}
}

// An account created under an address after the maintenance pass listed the
// accounts, while the address's old upload and private workspace trees are
// still there, keeps both trees with what it wrote since, and its approvals.
func TestMaintenanceKeepsAnAccountCreatedDuringTheSweep(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "live@x.com")
	s.sessionApprovals = NewSessionApprovalRegistry()
	att := t.TempDir()
	s.cfg.EmailAttachmentDir = att
	ctx := context.Background()
	uploads := accountdelete.UploadsDir(att, "back@x.com")
	stash := filepath.Join(accountdelete.PrivateWorkspaceRoot(att), "conv-1", "participants", accountdelete.Digest("back@x.com"))
	for _, d := range []string{filepath.Join(uploads, "old-tok"), stash} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	freshUpload := filepath.Join(uploads, "fresh-tok")
	freshStash := filepath.Join(stash, "notes.txt")
	s.afterAccountSnapshot = func() {
		seedUser(t, s.concreteStore(t), "back@x.com")
		if err := os.MkdirAll(freshUpload, 0o700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(freshStash, []byte("x"), 0o600); err != nil {
			t.Error(err)
		}
		s.sessionApprovals.RegisterForUser("conv-1", "back@x.com", "run_python", SessionApprovalPolicy{Mode: "approve"})
	}
	s.sweepDeletedAccounts(ctx)
	for _, f := range []string{freshUpload, freshStash} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s of the account created during the sweep was removed: %v", f, err)
		}
	}
	if _, ok := s.sessionApprovals.MatchForUser("conv-1", "back@x.com", "run_python", "{}"); !ok {
		t.Error("the approval of the account created during the sweep was dropped")
	}
}

// An upload tree an interrupted removal set aside is removed on the next
// pass even when no tree is orphaned; a live account's tree stays.
func TestMaintenanceRemovesSetAsideUploadsOnEveryPass(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "live@x.com")
	att := t.TempDir()
	s.cfg.EmailAttachmentDir = att
	liveTree := filepath.Join(accountdelete.UploadsDir(att, "live@x.com"), "tok")
	aside := filepath.Join(att, "uploads", ".removing-"+accountdelete.Digest("gone@x.com")+"-0123", "tok")
	for _, d := range []string{liveTree, aside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s.sweepDeletedAccounts(context.Background())
	if _, err := os.Stat(filepath.Dir(aside)); !os.IsNotExist(err) {
		t.Errorf("the set-aside tree survived a pass with no orphan (err %v)", err)
	}
	if _, err := os.Stat(liveTree); err != nil {
		t.Errorf("the live account's uploads were removed: %v", err)
	}
}

// The store's one-digest check (the sweep's last look before a removal)
// hashes an account's email as accountdelete.Digest names its trees.
func TestStoreDigestCheckMatchesAccountDeletion(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "Mixed.Case@X.com")
	st := s.concreteStore(t)
	ctx := context.Background()
	if live, err := st.AccountDigestLive(ctx, accountdelete.Digest(" Mixed.Case@X.com ")); err != nil || !live {
		t.Fatalf("the account's own digest: live %v, err %v; want live", live, err)
	}
	if live, err := st.AccountDigestLive(ctx, accountdelete.Digest("nobody@x.com")); err != nil || live {
		t.Fatalf("an address with no account: live %v, err %v; want not live", live, err)
	}
}

// A write the store refuses because the account no longer exists — it was
// deleted (by the CLI, say) after the request was admitted — is answered as
// any later request of the account is: 403 not_a_member, not a 500.
func TestWriteForAnAccountDeletedMidRequestIsNotAMember(t *testing.T) {
	t.Parallel()
	s := serverFixture(t)
	// Admitted, as before the deletion; the account row is already gone.
	s.isMember = func(context.Context, string) (bool, error) { return true, nil }
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/conversations", map[string]any{"persona": "victoria"}},
		{"/memories", map[string]any{"content": "a late fact"}},
		{"/user-skills", map[string]any{"name": "late-skill", "description": "d", "body": "b"}},
	} {
		w := do(t, s.Routes(), http.MethodPost, c.path, c.body, "gone@x.com")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_a_member") {
			t.Errorf("POST %s: status %d body %q, want 403 not_a_member", c.path, w.Code, w.Body.String())
		}
	}
}

// Deleting an account stops the turns it has running — here one in its own
// chat — and waits for them; another person's turn keeps running.
func TestStopAccountWorkCancelsItsTurns(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com", "mate@x.com")
	ctx := context.Background()
	own, err := s.store.CreateConversation(ctx, "u@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	mates, err := s.store.CreateConversation(ctx, "mate@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := map[string]bool{}
	register := func(convID string) {
		s.inflightMu.Lock()
		defer s.inflightMu.Unlock()
		s.inflight[convID] = inflightEntry{cancel: func() {
			// The turn goroutine notices and ends a moment later.
			go func() {
				time.Sleep(20 * time.Millisecond)
				s.inflightMu.Lock()
				e := s.inflight[convID]
				e.finishedAt = time.Now()
				s.inflight[convID] = e
				cancelled[convID] = true
				s.inflightMu.Unlock()
			}()
		}}
	}
	register(own.ID)
	register(mates.ID)
	if stopped, err := s.stopAccountWork(ctx, "U@x.com"); !stopped || err != nil {
		t.Fatalf("stopAccountWork = %v, %v; want stopped", stopped, err)
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if !cancelled[own.ID] {
		t.Error("the turn in the account's own chat was not stopped")
	}
	if cancelled[mates.ID] {
		t.Error("another person's turn was stopped")
	}
}

// Two first requests at once with one key minted before keys named their
// account: both are admitted and the key ends bound once. Run under -race:
// the edge reads its own copy of the key record while a request binds it.
func TestConcurrentFirstUsesOfAnUnboundKey(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	mgr, userKey, _ := edgeKeys(t)
	s.apiKeys = mgr
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			errs <- chattui.NewClient(chattui.Config{ServerURL: srv.URL, APIKey: userKey}).CheckIdentity(context.Background())
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Errorf("first use: %v", err)
		}
	}
	u, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mgr.ListKeys() {
		if k.UserEmail == "u@x.com" && (k.AccountID != u.ID || k.AccountMissing) {
			t.Fatalf("key = %+v, want bound to %s", k, u.ID)
		}
	}
}

// Stopping the account's work gives up as soon as the deleting request ends,
// instead of waiting out the full drain time.
func TestStopAccountWorkReturnsWhenItsRequestEnds(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	// A request of the account that never ends on its own.
	_, leave, _ := s.accounts.enter(context.Background(), "u@x.com")
	defer leave()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	stopped, err := s.stopAccountWork(ctx, "u@x.com")
	if err == nil || stopped || time.Since(start) > 5*time.Second {
		t.Fatalf("stopAccountWork = %v, %v after %v; want the context's error at once", stopped, err, time.Since(start))
	}
}

// Two administrators deleting each other at once: each deletion cancels the
// other's request, and neither hangs for the drain time or answers 500.
func TestAdminsDeletingEachOther(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "a@x.com", "b@x.com")
	setRole(t, s, "a@x.com", "admin", "")
	setRole(t, s, "b@x.com", "admin", "")
	h := s.Routes()
	type answer struct {
		code int
		took time.Duration
	}
	answers := make(chan answer, 2)
	for _, pair := range [][2]string{{"a@x.com", "b@x.com"}, {"b@x.com", "a@x.com"}} {
		go func(actor, target string) {
			start := time.Now()
			w := do(t, h, http.MethodDelete, "/admin/users/"+target, nil, actor)
			answers <- answer{w.Code, time.Since(start)}
		}(pair[0], pair[1])
	}
	for i := 0; i < 2; i++ {
		a := <-answers
		if a.took > defaultAccountDrainTimeout-time.Second {
			t.Errorf("a deletion took %v", a.took)
		}
		if a.code != http.StatusNoContent && a.code != http.StatusServiceUnavailable && a.code != http.StatusForbidden && a.code != http.StatusNotFound {
			t.Errorf("status %d, want 204, or 503 (interrupted), 403 or 404 for the one that lost", a.code)
		}
	}
}

// Once the account's work is stopped, the deletion runs to its end even when
// the client goes away: the account is not left stopped but still there.
func TestAdminDeleteFinishesAfterTheClientLeaves(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	ctx, cancel := context.WithCancel(asUser(context.Background(), "boss@x.com"))
	s.afterAccountWorkStopped = cancel // the client leaves right after the stop
	req := httptest.NewRequestWithContext(ctx, http.MethodDelete, "/admin/users/u%40x.com", nil)
	w := httptest.NewRecorder()
	s.handleAdminUserDelete(w, req, "u@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if _, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com"); err == nil {
		t.Fatal("the account survived a client that left after its work was stopped")
	}
}

// A client that leaves before the work is stopped deletes nothing.
func TestAdminDeleteInterruptedBeforeTheStopDeletesNothing(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	_, leave, _ := s.accounts.enter(context.Background(), "u@x.com") // a request of the account still running
	ctx, cancel := context.WithCancel(asUser(context.Background(), "boss@x.com"))
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	req := httptest.NewRequestWithContext(ctx, http.MethodDelete, "/admin/users/u%40x.com", nil)
	w := httptest.NewRecorder()
	s.handleAdminUserDelete(w, req, "u@x.com")
	leave()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d body %s, want 503", w.Code, w.Body.String())
	}
	if _, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com"); err != nil {
		t.Fatalf("the account was deleted: %v", err)
	}
	if _, l, ok := s.accounts.enter(context.Background(), "u@x.com"); !ok {
		t.Fatal("the account's requests stay refused after an interrupted deletion")
	} else {
		l()
	}
}

// A deletion that goes ahead with some of the account's work still running
// after the wait says so in its warnings.
func TestAdminDeleteWarnsOfWorkStillRunning(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	s.accountDrain = time.Second
	// A request of the account that does not end when cancelled.
	_, leave, _ := s.accounts.enter(context.Background(), "u@x.com")
	defer leave()
	w := do(t, s.Routes(), http.MethodDelete, "/admin/users/u@x.com", nil, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s, want 200 with warnings", w.Code, w.Body.String())
	}
	var body struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Warnings) != 1 ||
		body.Warnings[0] != errAccountWorkStillRunning(time.Second).Error() {
		t.Fatalf("warnings = %q (err %v)", body.Warnings, err)
	}
	if _, err := s.concreteStore(t).GetUser(context.Background(), "u@x.com"); err == nil {
		t.Fatal("the account survived")
	}
}

// blockingDeleteStore is the real store with a DeleteUser that never finishes
// on its own: it returns only when its context ends.
type blockingDeleteStore struct{ *store.Store }

func (blockingDeleteStore) DeleteUser(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// A deletion that gets stuck after the account's work stopped is bounded: it
// gives up when its time is out, the account stays, and the account's
// requests are served again.
func TestAdminDeleteIsBounded(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	st := s.concreteStore(t)
	s.store = blockingDeleteStore{st}
	s.accountDeleteLimit = 300 * time.Millisecond
	req := httptest.NewRequestWithContext(asUser(context.Background(), "boss@x.com"), http.MethodDelete, "/admin/users/u%40x.com", nil)
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleAdminUserDelete(w, req, "u@x.com")
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the stuck deletion held on for %v", took)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d body %s, want 500", w.Code, w.Body.String())
	}
	if _, err := st.GetUser(context.Background(), "u@x.com"); err != nil {
		t.Fatalf("the account is gone: %v", err)
	}
	if _, l, ok := s.accounts.enter(context.Background(), "u@x.com"); !ok {
		t.Fatal("the account's requests stay refused after the deletion gave up")
	} else {
		l()
	}
}

// slowDirectory is an account lookup that takes until its context ends.
type slowDirectory struct{ started chan struct{} }

func (d slowDirectory) GetByEmail(ctx context.Context, _ string) (*identity.User, error) {
	close(d.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (slowDirectory) LookupExternalSessionEpoch(context.Context, string, string) (string, error) {
	return "", nil
}

// A request whose account lookup a deletion cuts short is answered as not a
// member, not as a server failure.
func TestLookupCutShortByADeletionIsNotAMember(t *testing.T) {
	t.Parallel()
	s := memberFixture(t, "u@x.com")
	dir := slowDirectory{started: make(chan struct{})}
	s.dir = dir
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(t, s.Routes(), http.MethodGet, "/auth/membership", nil, "u@x.com") }()
	<-dir.started
	reopen := s.accounts.close("u@x.com")
	defer reopen()
	w := <-done
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not_a_member") {
		t.Fatalf("status %d body %s, want 403 not_a_member", w.Code, w.Body.String())
	}
}
