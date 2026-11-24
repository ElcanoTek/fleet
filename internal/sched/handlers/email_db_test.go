package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// setupEmailTest builds a router with only the email-trigger route, against the
// shared test Postgres. Skips when the DB is unavailable.
func setupEmailTest(t *testing.T) (*chi.Mux, *storage.Storage, func()) {
	t.Helper()
	r, store, _, cleanup := setupEmailTestDSN(t)
	return r, store, cleanup
}

// setupEmailTestDSN is setupEmailTest that also returns the scheduler DSN, so a
// test can hold locks on a connection of its own, outside the store's pool.
func setupEmailTestDSN(t *testing.T) (*chi.Mux, *storage.Storage, string, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "email-trig-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	dsn := fleetdb.New(t).SchedDSN()
	store := storage.New()
	if err := store.Initialize(dsn, fleetdb.SchedPool()); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("init storage: %v", err)
	}

	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "api_keys.json"), filepath.Join(tmpDir, "audit_log.jsonl"))
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("key mgr: %v", err)
	}

	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Post("/triggers/email/{slug}", h.HandleEmailTrigger)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return r, store, dsn, cleanup
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// seedEmailTrigger creates a template task + an email trigger and returns the
// trigger secret. allowEvent sets the template's connector opt-in; mcp is the
// template's MCP selection (to prove connector gating).
func seedEmailTrigger(t *testing.T, store *storage.Storage, slug string, policy *models.EmailTriggerPolicy, allowEvent bool, mcp models.MCPSelection) (secret string) {
	t.Helper()
	task := &models.Task{
		ID:                 uuid.New(),
		Prompt:             "handle inbound mail",
		Status:             models.TaskStatusScheduled,
		Priority:           models.PriorityNormal,
		TriggerType:        models.TriggerTypeWebhook,
		AllowEventTriggers: allowEvent,
		MCPSelection:       mcp,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	secret = "test-email-trigger-secret-0123456789abcdef"
	trig := &models.TaskTrigger{
		ID:          uuid.New(),
		TaskID:      task.ID,
		Slug:        slug,
		Secret:      secret,
		Kind:        models.TriggerKindEmail,
		EmailPolicy: policy,
	}
	if err := store.CreateTrigger(context.Background(), trig); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	return secret
}

func postEmail(r *chi.Mux, slug, secret string, email InboundEmail) *httptest.ResponseRecorder {
	return postEmailCtx(context.Background(), r, slug, secret, email)
}

// postEmailCtx is postEmail with the request bound to ctx, so a test can drop
// the request part-way, as a sender that stops waiting does.
func postEmailCtx(ctx context.Context, r *chi.Mux, slug, secret string, email InboundEmail) *httptest.ResponseRecorder {
	body, _ := json.Marshal(email)
	req := httptest.NewRequestWithContext(ctx, "POST", "/triggers/email/"+slug, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func validPolicy() *models.EmailTriggerPolicy {
	return &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: true, MaxAttachments: 0}
}

func goodEmail() InboundEmail {
	return InboundEmail{MessageID: "<msg-1@corp.com>", From: "alerts@corp.com", To: "task-x@inbound.fleet", Subject: "Deploy", Text: "please deploy", DKIM: "pass", SPF: "pass"}
}

func TestHandleEmailTrigger_HappyPath(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "hp", validPolicy(), false, nil)

	w := postEmail(r, "hp", secret, goodEmail())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	runID, err := uuid.Parse(resp.RunID)
	if err != nil {
		t.Fatalf("bad run_id %q: %v", resp.RunID, err)
	}
	run, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("spawned run not found: %v", err)
	}
	// The spawned run's prompt carries the email content (default prompt).
	if run.Prompt == "" || run.Status != models.TaskStatusPending {
		t.Errorf("unexpected spawned run: status=%s prompt=%q", run.Status, run.Prompt)
	}
	// The event is linked to the run for reply-back / audit.
	ev, err := store.GetTriggerEventByRunID(context.Background(), runID)
	if err != nil {
		t.Fatalf("event not linked to run: %v", err)
	}
	if ev.Sender != "alerts@corp.com" || ev.MessageID != "<msg-1@corp.com>" {
		t.Errorf("event reply metadata wrong: %+v", ev)
	}
}

func TestHandleEmailTrigger_BadSignature(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	seedEmailTrigger(t, store, "bad", validPolicy(), false, nil)

	w := postEmail(r, "bad", "wrong-secret-wrong-secret-wrong-secret", goodEmail())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", w.Code)
	}
}

func TestHandleEmailTrigger_UnknownSlug(t *testing.T) {
	t.Parallel()

	r, _, cleanup := setupEmailTest(t)
	defer cleanup()
	// No trigger seeded — any signature fails closed with 401 (no enumeration).
	w := postEmail(r, "nope", "some-secret-some-secret-some-secret-xx", goodEmail())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown slug: status = %d, want 401", w.Code)
	}
}

func TestHandleEmailTrigger_WrongKindSlug(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	// Seed a WEBHOOK-kind trigger, then hit the email endpoint: must be an
	// identical 401 (kind must not leak), even with a correct signature.
	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusScheduled, Priority: models.PriorityNormal, TriggerType: models.TriggerTypeWebhook}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	secret := "webhook-kind-secret-webhook-kind-secret"
	if err := store.CreateTrigger(context.Background(), &models.TaskTrigger{ID: uuid.New(), TaskID: task.ID, Slug: "wh", Secret: secret, Kind: models.TriggerKindWebhook}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	w := postEmail(r, "wh", secret, goodEmail())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("webhook-kind slug on email endpoint: status = %d, want 401", w.Code)
	}
}

func TestHandleEmailTrigger_UnapprovedSender(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "sndr", validPolicy(), false, nil)

	e := goodEmail()
	e.From = "attacker@evil.com"
	w := postEmail(r, "sndr", secret, e)
	if w.Code != http.StatusForbidden {
		t.Errorf("unapproved sender: status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
}

func TestHandleEmailTrigger_DKIMFail(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "dkim", validPolicy(), false, nil)

	e := goodEmail()
	e.DKIM = "fail"
	w := postEmail(r, "dkim", secret, e)
	if w.Code != http.StatusForbidden {
		t.Errorf("DKIM fail: status = %d, want 403", w.Code)
	}
}

func TestHandleEmailTrigger_AttachmentTooLarge(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	pol := &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: true, MaxAttachments: 1, MaxAttachmentBytes: 100}
	secret := seedEmailTrigger(t, store, "att", pol, false, nil)

	e := goodEmail()
	e.Attachments = []InboundAttachment{{Filename: "big.pdf", Size: 999}}
	w := postEmail(r, "att", secret, e)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized attachment: status = %d, want 413 (%s)", w.Code, w.Body.String())
	}
}

func TestHandleEmailTrigger_Dedup(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "dup", validPolicy(), false, nil)

	if w := postEmail(r, "dup", secret, goodEmail()); w.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202", w.Code)
	}
	// Same Message-ID again: idempotent no-op, 200, NO second run.
	w := postEmail(r, "dup", secret, goodEmail())
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate delivery: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "duplicate" {
		t.Errorf("duplicate response = %v, want status=duplicate", resp)
	}
	// Exactly one run was spawned.
	pending, err := store.GetPendingTasks()
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	spawned := 0
	for _, tk := range pending {
		if tk.TriggerType == models.TriggerTypeCron { // spawned runs are one-shot cron-type
			spawned++
		}
	}
	if spawned != 1 {
		t.Errorf("expected exactly 1 spawned run after a duplicate, got %d", spawned)
	}
}

// TestHandleEmailTrigger_RefusesUnstorableKeys: a Message-ID the database
// cannot hold as given (U+0000, or longer than an RFC 5322 line) is refused
// with 400 before any lookup, on the first delivery and every retry alike,
// rather than failing that lookup with a 500 each time; a From holding U+0000
// is refused as an unapproved sender. The sender and attachment checks still
// answer first. Nothing is recorded and no run starts.
func TestHandleEmailTrigger_RefusesUnstorableKeys(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "badkeys", validPolicy(), false, nil)
	conn := store.DB().Conn()

	for _, tc := range []struct {
		name   string
		mutate func(*InboundEmail)
		want   int
	}{
		{"Message-ID with U+0000", func(e *InboundEmail) { e.MessageID = "<msg\x00-1@corp.com>" }, http.StatusBadRequest},
		{"Message-ID longer than a line", func(e *InboundEmail) {
			e.MessageID = "<" + strings.Repeat("m", maxMessageIDBytes) + "@corp.com>"
		}, http.StatusBadRequest},
		{"From with U+0000", func(e *InboundEmail) { e.From = "alerts@corp.com\x00" }, http.StatusForbidden},
		{"From display name with U+0000", func(e *InboundEmail) { e.From = "Al\x00erts <alerts@corp.com>" }, http.StatusForbidden},
		// The sender and attachment checks answer first, as for any email.
		{"unapproved sender, Message-ID with U+0000", func(e *InboundEmail) {
			e.From, e.MessageID = "someone@elsewhere.com", "<msg\x00-2@corp.com>"
		}, http.StatusForbidden},
		{"oversize attachment, Message-ID with U+0000", func(e *InboundEmail) {
			e.MessageID = "<msg\x00-3@corp.com>"
			e.Attachments = []InboundAttachment{{Filename: "a.pdf", Size: 10}}
		}, http.StatusRequestEntityTooLarge},
	} {
		e := goodEmail()
		tc.mutate(&e)
		for attempt := 1; attempt <= 2; attempt++ {
			if w := postEmail(r, "badkeys", secret, e); w.Code != tc.want {
				t.Errorf("%s, delivery %d: status = %d, want %d (%s)", tc.name, attempt, w.Code, tc.want, w.Body.String())
			}
		}
	}
	if n := emailEventCount(t, conn); n != 0 {
		t.Errorf("refused deliveries left %d trigger_events row(s), want 0", n)
	}
	var runs int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sched.tasks WHERE COALESCE(trigger_type, '') <> 'webhook'`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("refused deliveries started %d run(s), want 0", runs)
	}

	// A Message-ID at the limit is still accepted.
	ok := goodEmail()
	ok.MessageID = strings.Repeat("m", maxMessageIDBytes)
	if w := postEmail(r, "badkeys", secret, ok); w.Code != http.StatusAccepted {
		t.Fatalf("Message-ID of exactly %d bytes: status = %d, want 202 (%s)", maxMessageIDBytes, w.Code, w.Body.String())
	}
}

// TestHandleEmailTrigger_DuplicateAfterTemplateBreaks: a delivery that already
// ran stays a duplicate when its trigger's template is changed afterwards to
// one that no longer renders. The redelivery is answered 200 duplicate, not
// the 400 a new email now gets, and starts no run.
func TestHandleEmailTrigger_DuplicateAfterTemplateBreaks(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "tmplbreak", validPolicy(), false, nil)
	conn := store.DB().Conn()

	first := postEmail(r, "tmplbreak", secret, goodEmail())
	if first.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202 (%s)", first.Code, first.Body.String())
	}
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE sched.task_triggers SET prompt_template = '{{.Subject' WHERE slug = 'tmplbreak'`); err != nil {
		t.Fatalf("break the template: %v", err)
	}

	w := postEmail(r, "tmplbreak", secret, goodEmail())
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"duplicate"`)) {
		t.Fatalf("redelivery after the template broke: status = %d body %s, want 200 duplicate", w.Code, w.Body.String())
	}
	// The template really is broken: a new email is refused with 400.
	fresh := goodEmail()
	fresh.MessageID = "<msg-2@corp.com>"
	if w := postEmail(r, "tmplbreak", secret, fresh); w.Code != http.StatusBadRequest {
		t.Fatalf("new email with the broken template: status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	assertOneLinkedEmailRun(t, store, conn, first)
}

// TestHandleEmailTrigger_FailedSpawnIsNotRecorded: a delivery whose run cannot
// be created is not recorded, so the sender's retry of the same Message-ID
// starts the run instead of being answered as a duplicate.
func TestHandleEmailTrigger_FailedSpawnIsNotRecorded(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "failspawn", validPolicy(), false, nil)
	conn := store.DB().Conn()
	ctx := context.Background()

	// Refuse any new task whose prompt carries the email body, so the run
	// insert fails the way a database error would. NOT VALID leaves the
	// template row unchecked.
	if _, err := conn.ExecContext(ctx, `ALTER TABLE sched.tasks ADD CONSTRAINT refuse_email_runs CHECK (prompt NOT LIKE '%please deploy%') NOT VALID`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	if w := postEmail(r, "failspawn", secret, goodEmail()); w.Code != http.StatusInternalServerError {
		t.Fatalf("delivery whose run insert fails: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if n := emailEventCount(t, conn); n != 0 {
		t.Fatalf("a delivery that started no run left %d trigger_events row(s), want 0", n)
	}

	if _, err := conn.ExecContext(ctx, `ALTER TABLE sched.tasks DROP CONSTRAINT refuse_email_runs`); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	w := postEmail(r, "failspawn", secret, goodEmail())
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry of the same Message-ID: status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	assertOneLinkedEmailRun(t, store, conn, w)
}

// TestHandleEmailTrigger_DroppedDeliveryIsNotRecorded: the request ends while
// the run is being created — the sender stopped waiting, or fleet stopped —
// after the delivery's record was written but before its run was. Nothing of
// it is kept, so the retry starts the run.
func TestHandleEmailTrigger_DroppedDeliveryIsNotRecorded(t *testing.T) {
	t.Parallel()

	r, store, dsn, cleanup := setupEmailTestDSN(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "dropped", validPolicy(), false, nil)
	side := openSideDB(t, dsn)

	release := holdTaskInserts(t, side)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dropped := make(chan *httptest.ResponseRecorder, 1)
	go func() { dropped <- postEmailCtx(reqCtx, r, "dropped", secret, goodEmail()) }()
	waitUntil(t, "the delivery waits inside its run insert", func() bool { return lockWaits(t, side) >= 1 })
	cancel()
	release()
	if w := <-dropped; w.Code != http.StatusInternalServerError {
		t.Fatalf("dropped delivery: status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if n := emailEventCount(t, side); n != 0 {
		t.Fatalf("a dropped delivery left %d trigger_events row(s), want 0", n)
	}

	w := postEmail(r, "dropped", secret, goodEmail())
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry of the same Message-ID: status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	assertOneLinkedEmailRun(t, store, side, w)
}

// TestHandleEmailTrigger_ConcurrentDuplicatesRunOnce: two deliveries of the
// same Message-ID in flight together start exactly one run. The second is held
// until the first is decided, then answered as a duplicate.
func TestHandleEmailTrigger_ConcurrentDuplicatesRunOnce(t *testing.T) {
	t.Parallel()

	r, store, dsn, cleanup := setupEmailTestDSN(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "concurrent", validPolicy(), false, nil)
	side := openSideDB(t, dsn)

	release := holdTaskInserts(t, side)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postEmail(r, "concurrent", secret, goodEmail()) }()
	waitUntil(t, "the first delivery waits inside its run insert", func() bool { return lockWaits(t, side) >= 1 })
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- postEmail(r, "concurrent", secret, goodEmail()) }()
	secondResp := waitForSecondDelivery(t, side, second)
	release()

	w1 := <-first
	if secondResp == nil {
		secondResp = <-second
	}
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first delivery: status = %d, want 202 (%s)", w1.Code, w1.Body.String())
	}
	if secondResp.Code != http.StatusOK || !bytes.Contains(secondResp.Body.Bytes(), []byte(`"duplicate"`)) {
		t.Fatalf("concurrent duplicate: status = %d body %s, want 200 duplicate", secondResp.Code, secondResp.Body.String())
	}
	assertOneLinkedEmailRun(t, store, side, w1)
}

// TestHandleEmailTrigger_ConcurrentRetryRunsWhenFirstFails: while one delivery
// is creating its run, a second delivery of the same Message-ID waits for it.
// When the first ends without a run, the second starts the run rather than
// being answered as a duplicate of a delivery that never ran.
func TestHandleEmailTrigger_ConcurrentRetryRunsWhenFirstFails(t *testing.T) {
	t.Parallel()

	r, store, dsn, cleanup := setupEmailTestDSN(t)
	defer cleanup()
	secret := seedEmailTrigger(t, store, "firstfails", validPolicy(), false, nil)
	side := openSideDB(t, dsn)

	release := holdTaskInserts(t, side)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postEmailCtx(firstCtx, r, "firstfails", secret, goodEmail()) }()
	waitUntil(t, "the first delivery waits inside its run insert", func() bool { return lockWaits(t, side) >= 1 })
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- postEmail(r, "firstfails", secret, goodEmail()) }()
	secondResp := waitForSecondDelivery(t, side, second)
	cancelFirst()
	release()

	if w1 := <-first; w1.Code != http.StatusInternalServerError {
		t.Fatalf("dropped first delivery: status = %d, want 500 (%s)", w1.Code, w1.Body.String())
	}
	if secondResp == nil {
		secondResp = <-second
	}
	if secondResp.Code != http.StatusAccepted {
		t.Fatalf("second delivery after the first ended without a run: status = %d, want 202 (%s)", secondResp.Code, secondResp.Body.String())
	}
	assertOneLinkedEmailRun(t, store, side, secondResp)
}

// openSideDB opens a connection pool of the test's own on the scheduler
// database, outside the store's small pool, for holding locks and watching.
func openSideDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	side, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open side connection: %v", err)
	}
	// One connection holds the lock, one watches; the test server's
	// connections are shared with every other parallel suite.
	side.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = side.Close() })
	return side
}

// holdTaskInserts takes a lock on sched.tasks that plain reads pass but a new
// task row waits on, so a delivery stops inside its run insert until release.
func holdTaskInserts(t *testing.T, side *sql.DB) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := side.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE sched.tasks IN EXCLUSIVE MODE`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("lock sched.tasks: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// lockWaits counts the sessions on this test database waiting for a lock.
func lockWaits(t *testing.T, side *sql.DB) int {
	t.Helper()
	var n int
	if err := side.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		t.Fatalf("count lock waits: %v", err)
	}
	return n
}

// waitForSecondDelivery waits until the second delivery is either waiting on
// a lock behind the first, or already answered (returned, else nil).
func waitForSecondDelivery(t *testing.T, side *sql.DB, second <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var resp *httptest.ResponseRecorder
	waitUntil(t, "the second delivery waits behind the first or is answered", func() bool {
		select {
		case resp = <-second:
			return true
		default:
		}
		return lockWaits(t, side) >= 2
	})
	return resp
}

// waitUntil polls cond until it holds, failing the test after a while.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// emailEventCount counts the recorded deliveries (trigger_events rows).
func emailEventCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM sched.trigger_events`).Scan(&n); err != nil {
		t.Fatalf("count trigger_events: %v", err)
	}
	return n
}

// assertOneLinkedEmailRun checks that exactly one run and one recorded
// delivery exist, that w answered with that run, and that the delivery is
// linked to it.
func assertOneLinkedEmailRun(t *testing.T, store *storage.Storage, conn *sql.DB, w *httptest.ResponseRecorder) {
	t.Helper()
	run := runFromResp(t, store, w)
	var runs int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sched.tasks WHERE COALESCE(trigger_type, '') <> 'webhook'`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("runs = %d, want exactly 1", runs)
	}
	if n := emailEventCount(t, conn); n != 1 {
		t.Errorf("trigger_events rows = %d, want exactly 1", n)
	}
	ev, err := store.GetTriggerEventByRunID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("the delivery is not linked to run %s: %v", run.ID, err)
	}
	if ev.MessageID != goodEmail().MessageID {
		t.Errorf("linked delivery message id = %q, want %q", ev.MessageID, goodEmail().MessageID)
	}
}

func TestHandleEmailTrigger_ConnectorOptIn(t *testing.T) {
	t.Parallel()

	mcp := models.MCPSelection{{Server: "github", Account: "default"}}
	r, store, cleanup := setupEmailTest(t)
	defer cleanup()

	// Opt-out (default): spawned run inherits NO connectors.
	secretOut := seedEmailTrigger(t, store, "optout", validPolicy(), false, mcp)
	wOut := postEmail(r, "optout", secretOut, goodEmail())
	if wOut.Code != http.StatusAccepted {
		t.Fatalf("opt-out status = %d, want 202", wOut.Code)
	}
	if run := runFromResp(t, store, wOut); len(run.MCPSelection) != 0 {
		t.Errorf("opt-out run should have NO connectors, got %+v", run.MCPSelection)
	}

	// Opt-in: spawned run inherits the template's connectors. Distinct slug +
	// Message-ID so it is a fresh event (not deduped against the opt-out one).
	secretIn := seedEmailTrigger(t, store, "optin", validPolicy(), true, mcp)
	e := goodEmail()
	e.MessageID = "<msg-optin@corp.com>"
	wIn := postEmail(r, "optin", secretIn, e)
	if wIn.Code != http.StatusAccepted {
		t.Fatalf("opt-in status = %d, want 202", wIn.Code)
	}
	run := runFromResp(t, store, wIn)
	if len(run.MCPSelection) != 1 || run.MCPSelection[0].Server != "github" {
		t.Errorf("opt-in run should inherit connectors, got %+v", run.MCPSelection)
	}
}

// TestHandleEmailTrigger_OptOutRunReachesZeroConnectorSeats asserts the NEGATIVE
// the opt-in test above never did (#979): that an allow_event_triggers=false
// spawn can reach no connector seat at all. It walks the boundary end to end —
// inbound email → spawned run → the agentcore Gate-3 verdict the run loop
// actually enforces — because the bug was invisible at every intermediate step:
// the run's allowlist was empty by len(), and empty by len() was ALL seats, since
// a nil allowlist means "inherit global". Asserting the run merely "inherited
// nothing" is what let the code and its own comment drift apart.
func TestHandleEmailTrigger_OptOutRunReachesZeroConnectorSeats(t *testing.T) {
	t.Parallel()

	r, store, cleanup := setupEmailTest(t)
	defer cleanup()

	mcp := models.MCPSelection{{Server: "ssp", Account: "client_a"}}
	secret := seedEmailTrigger(t, store, "zeroseats", validPolicy(), false, mcp)
	// Scope the TEMPLATE to a real seat, so "inherited nothing" and "may call
	// nothing" are distinguishable outcomes rather than the same empty list.
	trig, err := store.GetTriggerBySlug(context.Background(), models.TriggerKindEmail, "zeroseats")
	if err != nil {
		t.Fatalf("GetTriggerBySlug: %v", err)
	}
	tmplAllow := models.CredentialAllowlist{{Server: "ssp", Account: "client_a"}}
	if _, err := store.UpdateTaskCredentialAllowlist(context.Background(), trig.TaskID, tmplAllow); err != nil {
		t.Fatalf("UpdateTaskCredentialAllowlist: %v", err)
	}

	w := postEmail(r, "zeroseats", secret, goodEmail())
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	run := runFromResp(t, store, w)

	if run.CredentialAllowlist == nil {
		t.Fatal("opt-out run has a NIL credential allowlist — nil is inherit-global, i.e. EVERY seat")
	}
	// The same conversion the scheduled runner performs, then the same Gate-3
	// predicate the broker enforces on every MCP call.
	al := make(agentcore.CredentialAllowlist, 0, len(run.CredentialAllowlist))
	for _, e := range run.CredentialAllowlist {
		al = append(al, agentcore.CredentialAllowlistEntry{Server: e.Server, Account: e.Account})
	}
	if !al.DeniesAll() {
		t.Fatalf("opt-out run allowlist is not deny-all: %+v", al)
	}
	for _, seat := range []struct{ server, account string }{
		{"ssp", "client_a"}, // the template's own seat
		{"ssp", ""},         // the same server's default seat
		{"mailbox", ""},     // any other bundle connector
	} {
		if al.Permits(seat.server, seat.account) {
			t.Errorf("opt-out run may call (%q, %q); an untrusted email must reach ZERO seats", seat.server, seat.account)
		}
	}
}

func runFromResp(t *testing.T, store *storage.Storage, w *httptest.ResponseRecorder) *models.Task {
	t.Helper()
	var resp struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode run_id: %v", err)
	}
	id, err := uuid.Parse(resp.RunID)
	if err != nil {
		t.Fatalf("bad run_id: %v", err)
	}
	run, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("run not found: %v", err)
	}
	return run
}
