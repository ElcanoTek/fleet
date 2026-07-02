package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupEmailTest builds a router with only the email-trigger route, against the
// shared test Postgres. Skips when the DB is unavailable.
func setupEmailTest(t *testing.T) (*chi.Mux, *storage.Storage, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "email-trig-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		os.RemoveAll(tmpDir)
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping: database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	acquireTestLock(t, store)

	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "api_keys.json"), filepath.Join(tmpDir, "audit_log.jsonl"))
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("key mgr: %v", err)
	}

	// Clean shared tables (task_triggers/trigger_events cascade off tasks).
	ctx := context.Background()
	for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}

	h := New(Config{OrchestratorURL: "http://localhost:8000", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Post("/triggers/email/{slug}", h.HandleEmailTrigger)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return r, store, cleanup
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
	body, _ := json.Marshal(email)
	req := httptest.NewRequest("POST", "/triggers/email/"+slug, bytes.NewReader(body))
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
	r, store, cleanup := setupEmailTest(t)
	defer cleanup()
	seedEmailTrigger(t, store, "bad", validPolicy(), false, nil)

	w := postEmail(r, "bad", "wrong-secret-wrong-secret-wrong-secret", goodEmail())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad signature: status = %d, want 401", w.Code)
	}
}

func TestHandleEmailTrigger_UnknownSlug(t *testing.T) {
	r, _, cleanup := setupEmailTest(t)
	defer cleanup()
	// No trigger seeded — any signature fails closed with 401 (no enumeration).
	w := postEmail(r, "nope", "some-secret-some-secret-some-secret-xx", goodEmail())
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown slug: status = %d, want 401", w.Code)
	}
}

func TestHandleEmailTrigger_WrongKindSlug(t *testing.T) {
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

func TestHandleEmailTrigger_ConnectorOptIn(t *testing.T) {
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
