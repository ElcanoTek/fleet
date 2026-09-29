package accountevents

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// fakePlanes is both databases in memory: Chat accounts and enabled Ops roles.
type fakePlanes struct {
	chat    map[string]store.AccountAccess
	ops     map[string]string
	opsErr  error
	queued  []store.AccountEvent
	nextSeq int64
}

func newPlanes() *fakePlanes {
	return &fakePlanes{chat: map[string]store.AccountAccess{}, ops: map[string]string{}}
}

func (f *fakePlanes) AccountAccess(_ context.Context, email string) (store.AccountAccess, bool, error) {
	a, ok := f.chat[email]
	return a, ok, nil
}

func (f *fakePlanes) OpsRole(_ context.Context, email string) (string, error) {
	if f.opsErr != nil {
		return "", f.opsErr
	}
	if r, ok := f.ops[email]; ok {
		return r, nil
	}
	return "none", nil
}

func (f *fakePlanes) EnqueueAccountEvent(_ context.Context, ev store.AccountEvent) (store.AccountEvent, error) {
	f.nextSeq++
	ev.ID = f.nextSeq
	f.queued = append(f.queued, ev)
	return ev, nil
}

func TestCommitQueuesOnlyRealChanges(t *testing.T) {
	ctx := context.Background()
	p := newPlanes()
	rec := NewRecorder(p, p, p)
	const email = "carol@x.com"

	// Create: no account before, member + readonly after.
	c := rec.Begin(ctx, email)
	p.chat[email] = store.AccountAccess{Email: email, Role: store.RoleMember, Enabled: true}
	p.ops[email] = "readonly"
	if err := c.Commit(ctx, store.AccountEventSourceAdminUI, "boss@x.com"); err != nil {
		t.Fatal(err)
	}
	// Re-asserting the same state is not a change.
	c = rec.Begin(ctx, email)
	if err := c.Commit(ctx, store.AccountEventSourceAdminUI, "boss@x.com"); err != nil {
		t.Fatal(err)
	}
	if len(p.queued) != 1 {
		t.Fatalf("queued %d events, want 1 (no-op re-assert must not emit)", len(p.queued))
	}
	got := p.queued[0]
	if got.Type != store.AccountEventAccessChanged || !got.Enabled || got.ChatRole != "member" ||
		got.OpsRole != "readonly" || got.Source != "admin_ui" || got.Actor != "boss@x.com" {
		t.Fatalf("event = %+v", got)
	}

	// Delete.
	c = rec.Begin(ctx, email)
	delete(p.chat, email)
	delete(p.ops, email)
	if err := c.Commit(ctx, store.AccountEventSourceCLI, ""); err != nil {
		t.Fatal(err)
	}
	if len(p.queued) != 2 || p.queued[1].Type != store.AccountEventDeleted ||
		p.queued[1].ChatRole != "" || p.queued[1].OpsRole != "" || p.queued[1].Enabled {
		t.Fatalf("delete event = %+v", p.queued[len(p.queued)-1])
	}

	// An email that was never a Chat account (an Ops-only automation user)
	// emits nothing even when its Ops role changes.
	c = rec.Begin(ctx, "robot")
	p.ops["robot"] = "client"
	if err := c.Commit(ctx, store.AccountEventSourceCLI, ""); err != nil || len(p.queued) != 2 {
		t.Fatalf("ops-only change queued (%v, %d events)", err, len(p.queued))
	}
}

// TestCommitReportsActualStateAfterFailedOpsWrite: the admin asked for Chat
// viewer + Ops client, the Ops write failed, so the event must carry the Ops
// role the database still holds, not the requested one.
func TestCommitReportsActualStateAfterFailedOpsWrite(t *testing.T) {
	ctx := context.Background()
	p := newPlanes()
	const email = "dan@x.com"
	p.chat[email] = store.AccountAccess{Email: email, Role: store.RoleMember, Enabled: true}
	p.ops[email] = "readonly"
	rec := NewRecorder(p, p, p)

	c := rec.Begin(ctx, email)
	p.chat[email] = store.AccountAccess{Email: email, Role: store.RoleViewer, Enabled: true}
	// (the Ops write to "client" failed: p.ops is untouched)
	if err := c.Commit(ctx, store.AccountEventSourceAdminUI, "boss@x.com"); err != nil {
		t.Fatal(err)
	}
	if len(p.queued) != 1 || p.queued[0].ChatRole != "viewer" || p.queued[0].OpsRole != "readonly" {
		t.Fatalf("queued %+v, want viewer + readonly", p.queued)
	}
}

func TestCommitRefusesToGuessWhenAPlaneIsUnreadable(t *testing.T) {
	ctx := context.Background()
	p := newPlanes()
	p.chat["e@x.com"] = store.AccountAccess{Email: "e@x.com", Role: store.RoleMember, Enabled: true}
	rec := NewRecorder(p, p, p)

	p.opsErr = errors.New("sched down")
	c := rec.Begin(ctx, "e@x.com")
	p.opsErr = nil
	p.ops["e@x.com"] = "admin"
	if err := c.Commit(ctx, "admin_ui", ""); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("commit after failed snapshot = %v, want ErrNotQueued", err)
	}
	c = rec.Begin(ctx, "e@x.com")
	p.opsErr = errors.New("sched down")
	if err := c.Commit(ctx, "admin_ui", ""); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("commit with failed read-back = %v, want ErrNotQueued", err)
	}
	if len(p.queued) != 0 {
		t.Fatalf("queued %+v from unknown state", p.queued)
	}
}

func TestNilRecorderIsOff(t *testing.T) {
	var rec *Recorder
	c := rec.Begin(context.Background(), "x@x.com")
	if err := c.Commit(context.Background(), "admin_ui", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := rec.Publish(context.Background(), "x@x.com", "resync", ""); ok || err != nil {
		t.Fatalf("nil Publish = (%v, %v)", ok, err)
	}
}

func TestPublishQueuesCurrentStateUnconditionally(t *testing.T) {
	ctx := context.Background()
	p := newPlanes()
	p.chat["r@x.com"] = store.AccountAccess{Email: "r@x.com", Role: store.RoleAdmin, Enabled: true}
	p.ops["r@x.com"] = "admin"
	rec := NewRecorder(p, p, p)
	for range 2 {
		if ok, err := rec.Publish(ctx, "r@x.com", store.AccountEventSourceResync, ""); !ok || err != nil {
			t.Fatalf("publish = (%v, %v)", ok, err)
		}
	}
	if ok, err := rec.Publish(ctx, "gone@x.com", store.AccountEventSourceResync, ""); ok || err != nil {
		t.Fatalf("publish of a missing account = (%v, %v), want skipped", ok, err)
	}
	if len(p.queued) != 2 || p.queued[1].Source != "resync" || p.queued[1].ChatRole != "admin" {
		t.Fatalf("queued %+v", p.queued)
	}
}

// fakeOutbox records the Deliverer's bookkeeping calls.
type fakeOutbox struct {
	mu        sync.Mutex
	due       []store.AccountEvent
	delivered []int64
	failed    []failedMark
	released  []int64
	pruned    int
}

type failedMark struct {
	id, now, retryAt int64
	giveUp           bool
	msg              string
}

func (f *fakeOutbox) ClaimDueAccountEvents(context.Context, int64, int, time.Duration) ([]store.AccountEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.due
	f.due = nil
	return out, nil
}

func (f *fakeOutbox) MarkAccountEventDelivered(_ context.Context, id, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, id)
	return nil
}

func (f *fakeOutbox) MarkAccountEventFailed(_ context.Context, id, now, retryAt int64, giveUp bool, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, failedMark{id, now, retryAt, giveUp, msg})
	return nil
}

func (f *fakeOutbox) ReleaseAccountEventLeases(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.released = append(f.released, ids...)
	return nil
}

func (f *fakeOutbox) PruneAccountEvents(context.Context, int64, int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned++
	return 0, nil
}

func sampleEvent(id int64, createdAt time.Time) store.AccountEvent {
	return store.AccountEvent{
		ID: id, EventID: "evt_abc", Type: store.AccountEventAccessChanged, Email: "p@x.com",
		Enabled: true, ChatRole: "member", OpsRole: "none", Source: "admin_ui", Actor: "boss@x.com",
		OccurredAt: 1790000000, CreatedAt: createdAt.Unix(), Attempts: 1,
	}
}

// TestDeliverySignatureMatchesWebhookSigningDoc recomputes the MAC exactly as
// docs/WEBHOOK-SIGNING.md tells a receiver to, over the raw bytes received.
func TestDeliverySignatureMatchesWebhookSigningDoc(t *testing.T) {
	const secret = "shared-secret"
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(42, time.Now())}}
	d := NewDeliverer(out, srv.URL+"/apps/fleet/events", secret)
	if err := d.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(out.delivered) != 1 || out.delivered[0] != 42 {
		t.Fatalf("delivered = %v, want [42]", out.delivered)
	}
	ts := gotHeader.Get("X-Fleet-Timestamp")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(gotBody)))
	if want := "v1=" + hex.EncodeToString(mac.Sum(nil)); gotHeader.Get("X-Fleet-Signature") != want {
		t.Fatalf("signature %q, want %q", gotHeader.Get("X-Fleet-Signature"), want)
	}
	if gotHeader.Get(EventIDHeader) != "evt_abc" || gotHeader.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", gotHeader)
	}
	const wantBody = `{"id":"evt_abc","type":"user.access_changed","occurred_at":1790000000,"sequence":42,` +
		`"source":"admin_ui","actor":"boss@x.com","user":{"email":"p@x.com","enabled":true,"chat_role":"member","ops_role":"none"}}`
	if string(gotBody) != wantBody {
		t.Fatalf("body\n got %s\nwant %s", gotBody, wantBody)
	}
}

func TestDeliveryRetriesNon2xxAndNeverFollowsRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
	redirected := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		redirected = true
		mu.Unlock()
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	out := &fakeOutbox{}
	d := NewDeliverer(out, srv.URL+"/hook?token=sekrit", "s")
	now := time.Now()
	for i := range 2 {
		ev := sampleEvent(7, now)
		ev.Attempts = i + 1
		out.due = []store.AccountEvent{ev}
		if err := d.RunOnce(context.Background(), now); err != nil {
			t.Fatal(err)
		}
	}
	if redirected {
		t.Fatal("deliverer followed a redirect with a signed body")
	}
	if len(out.delivered) != 0 || len(out.failed) != 2 {
		t.Fatalf("delivered %v failed %+v, want two failed attempts", out.delivered, out.failed)
	}
	for _, f := range out.failed {
		if f.giveUp || f.retryAt <= f.now || strings.Contains(f.msg, "sekrit") {
			t.Fatalf("failed mark %+v: want a scheduled retry and no URL query in the error", f)
		}
	}
	if !strings.Contains(out.failed[0].msg, "503") || !strings.Contains(out.failed[1].msg, "307") {
		t.Fatalf("messages = %q / %q", out.failed[0].msg, out.failed[1].msg)
	}
}

func TestDeliveryGivesUpAfterSevenDays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(9, time.Now().Add(-giveUpAfter-time.Minute))}}
	d := NewDeliverer(out, srv.URL, "s")
	if err := d.RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(out.failed) != 1 || !out.failed[0].giveUp {
		t.Fatalf("failed = %+v, want a give-up", out.failed)
	}
}

func TestDeliveryTransportErrorNamesHostOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/p?token=sekrit"
	srv.Close() // connection refused
	out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(3, time.Now())}}
	if err := NewDeliverer(out, url, "s").RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(out.failed) != 1 || strings.Contains(out.failed[0].msg, "sekrit") || strings.Contains(out.failed[0].msg, "/p") {
		t.Fatalf("failed = %+v, want a host-only error", out.failed)
	}
}

func TestRetryDelay(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second,
		10: 2560 * time.Second, 11: time.Hour, 50: time.Hour,
	} {
		if got := RetryDelay(attempts); got != want {
			t.Errorf("RetryDelay(%d) = %s, want %s", attempts, got, want)
		}
	}
}

func TestBodyForDeletedEvent(t *testing.T) {
	b, err := Body(store.AccountEvent{ID: 5, EventID: "evt_d", Type: store.AccountEventDeleted, Email: "z@x.com",
		Source: "cli", OccurredAt: 10})
	if err != nil {
		t.Fatal(err)
	}
	var p Payload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "user.deleted" || p.User.Enabled || p.User.ChatRole != "" || p.User.OpsRole != "" || p.Sequence != 5 {
		t.Fatalf("payload = %+v", p)
	}
}

// adoptingPlanes is fakePlanes that also holds the identity provider's stored
// desired state, the way the chat store does.
type adoptingPlanes struct {
	*fakePlanes
	adopted []adoption
}

type adoption struct {
	email             string
	exists            bool
	chatRole, opsRole string
}

func (a *adoptingPlanes) AdoptFleetAccessChange(_ context.Context, email string, exists bool, chatRole, opsRole string) error {
	a.adopted = append(a.adopted, adoption{email, exists, chatRole, opsRole})
	return nil
}

// TestCommitAdoptsFleetChangesIntoTheProviderBaseline: a change made in Fleet
// becomes the provider's stored baseline (so a redelivered, older push cannot
// revert it), a deletion marks it not allowed, and a change applied on the
// provider's own word is not adopted.
func TestCommitAdoptsFleetChangesIntoTheProviderBaseline(t *testing.T) {
	ctx := context.Background()
	p := &adoptingPlanes{fakePlanes: newPlanes()}
	rec := NewRecorder(p, p, p)
	const email = "fay@x.com"
	p.chat[email] = store.AccountAccess{Email: email, Role: store.RoleMember, Enabled: true}
	p.ops[email] = "readonly"

	c := rec.Begin(ctx, email)
	p.ops[email] = "client"
	if err := c.Commit(ctx, store.AccountEventSourceAdminUI, "boss@x.com"); err != nil {
		t.Fatal(err)
	}
	c = rec.Begin(ctx, email)
	p.ops[email] = "readonly"
	if err := c.Commit(ctx, store.AccountEventSourceIdentityProvider, ""); err != nil {
		t.Fatal(err)
	}
	c = rec.Begin(ctx, email)
	delete(p.chat, email)
	if err := c.Commit(ctx, store.AccountEventSourceCLI, ""); err != nil {
		t.Fatal(err)
	}
	want := []adoption{{email, true, "member", "client"}, {email, false, "", ""}}
	if len(p.adopted) != len(want) || p.adopted[0] != want[0] || p.adopted[1].exists {
		t.Fatalf("adopted = %+v, want %+v (the identity_provider change not adopted)", p.adopted, want)
	}
	if len(p.queued) != 3 {
		t.Fatalf("queued %d events, want 3", len(p.queued))
	}
}

func TestPublishDeletedQueuesOnlyForGoneAccounts(t *testing.T) {
	ctx := context.Background()
	p := newPlanes()
	p.chat["here@x.com"] = store.AccountAccess{Email: "here@x.com", Role: store.RoleMember, Enabled: true}
	rec := NewRecorder(p, p, p)
	if ok, err := rec.PublishDeleted(ctx, "here@x.com", store.AccountEventSourceResync, ""); ok || err != nil {
		t.Fatalf("existing account = (%v, %v), want skipped", ok, err)
	}
	if ok, err := rec.PublishDeleted(ctx, "gone@x.com", store.AccountEventSourceResync, ""); !ok || err != nil {
		t.Fatalf("gone account = (%v, %v), want queued", ok, err)
	}
	if len(p.queued) != 1 || p.queued[0].Type != store.AccountEventDeleted || p.queued[0].Email != "gone@x.com" ||
		p.queued[0].ChatRole != "" || p.queued[0].Enabled {
		t.Fatalf("queued %+v", p.queued)
	}
}

func TestLogSafeStripsLineBreaks(t *testing.T) {
	if got := logSafe("a@x.com\nforged line\r"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("logSafe = %q", got)
	}
}

// TestDeliveryGivesUpAtOnceOnARejectedBody: a receiver refusing the body
// itself cannot change its mind about the same bytes, so the row stops
// blocking the account's later events; an auth failure keeps retrying.
func TestDeliveryGivesUpAtOnceOnARejectedBody(t *testing.T) {
	for code, wantGiveUp := range map[int]bool{
		http.StatusBadRequest: true, http.StatusUnprocessableEntity: true, http.StatusConflict: true,
		http.StatusUnauthorized: false, http.StatusNotFound: false, http.StatusTooManyRequests: false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(5, time.Now())}}
		if err := NewDeliverer(out, srv.URL, "s").RunOnce(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if len(out.failed) != 1 || out.failed[0].giveUp != wantGiveUp {
			t.Errorf("status %d: failed = %+v, want giveUp=%v", code, out.failed, wantGiveUp)
		}
	}
}

// TestDeliveryReleasesLeasesWhenStopped: a stop that cuts a send off records
// no attempt and hands that row and every unsent one back, rather than leaving
// them leased past a restart.
func TestDeliveryReleasesLeasesWhenStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}))
	defer srv.Close()
	defer close(unblock)
	now := time.Now()
	out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(1, now), sampleEvent(2, now), sampleEvent(3, now)}}
	err := NewDeliverer(out, srv.URL, "s").RunOnce(ctx, now)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce = %v, want the cancellation", err)
	}
	if len(out.failed) != 0 || len(out.delivered) != 0 {
		t.Fatalf("failed %+v delivered %v, want nothing recorded for a cut-off send", out.failed, out.delivered)
	}
	if len(out.released) != 3 {
		t.Fatalf("released = %v, want all three claimed rows", out.released)
	}
}

// TestDelivererWithoutURLOnlyPrunes: with the feed off the retention sweep
// still runs, and nothing is claimed or sent.
func TestDelivererWithoutURLOnlyPrunes(t *testing.T) {
	out := &fakeOutbox{due: []store.AccountEvent{sampleEvent(1, time.Now())}}
	if err := NewDeliverer(out, "", "").RunOnce(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if out.pruned != 1 || len(out.due) != 1 || len(out.failed) != 0 {
		t.Fatalf("pruned %d due %d failed %+v, want a prune and no claim", out.pruned, len(out.due), out.failed)
	}
}
