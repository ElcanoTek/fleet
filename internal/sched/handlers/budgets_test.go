// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// DB-backed tests for per-principal rolling budgets (#601 part 2): the
// /admin/budgets CRUD surface and the shared create gate on BOTH in-process
// HTTP create paths (POST /tasks, POST /tasks/batch) — the chat schedule_task
// seam is covered in cmd/fleet, where it is wired. Spend is seeded as REAL
// task_iterations rows so enforcement is exercised over the same persisted
// metering the part-1 usage read model aggregates: hard-refuse, soft-alert
// exactly once per window crossing (surviving a "restart" — a fresh enforcer
// over the same DB), window rollover, the fail-safe clamp against the live
// global ceilings, and unchanged behavior with no budget configured.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// recordingNotifier captures budget soft alerts (the enforcer runs with
// SyncNotify in tests, so no polling is needed).
type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recordingNotifier) Notify(_ context.Context, ev notify.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}

// budgetTestEnv is one wired handler + enforcer over the shared test database,
// with the clock and the live global ceilings injectable per test.
type budgetTestEnv struct {
	r        *chi.Mux
	store    *storage.Storage
	keyMgr   *apikeys.Manager
	notifier *recordingNotifier
	now      time.Time
	ceilUSD  float64
	ceilTok  int
}

func setupBudgetEnv(t *testing.T) (*budgetTestEnv, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sched-budget-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		os.RemoveAll(tmpDir)
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("storage init: %v", err)
	}
	acquireTestLock(t, store)

	keyMgr, err := apikeys.NewManager(
		filepath.Join(tmpDir, "api_keys.json"),
		filepath.Join(tmpDir, "audit_log.jsonl"),
	)
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("key manager: %v", err)
	}

	ctx := context.Background()
	for _, q := range []string{"DELETE FROM budgets", "DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}

	env := &budgetTestEnv{
		store:    store,
		keyMgr:   keyMgr,
		notifier: &recordingNotifier{},
		// A fixed mid-window instant so seeded iterations land in the current
		// day/week/month deterministically. 2026-07-02 is a Thursday.
		now: time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}

	h := New(Config{DefaultTaskModel: "test/model",
		OrchestratorURL: "http://localhost:8000",
		AdminAPIKey:     "test-admin-key",
		Version:         "0.1.0",
	}, store, keyMgr)
	h.SetBudgetGate(env.newEnforcer())

	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	// The CRUD endpoints gate in-handler on PermissionAdmin (like
	// /admin/usage), so the test router registers them bare and calls them
	// with the admin API key.
	r.Get("/admin/budgets", h.ListBudgets)
	r.Post("/admin/budgets", h.UpsertBudget)
	r.Delete("/admin/budgets/{budget_id}", h.DeleteBudget)
	env.r = r

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return env, cleanup
}

// newEnforcer builds an enforcer over the env's live pointers — a SECOND call
// with the same env models a process restart (fresh memory, same database).
func (env *budgetTestEnv) newEnforcer() *budget.Enforcer {
	return budget.New(budget.Config{
		Store:      env.store,
		Notifier:   env.notifier,
		SyncNotify: true,
		Now:        func() time.Time { return env.now },
		Ceilings:   func() (float64, int) { return env.ceilUSD, env.ceilTok },
	})
}

// seedKeySpend persists a task attributed to keyID with one iteration of the
// given cost/tokens inside the current day window — real rows in the same
// tables the part-1 usage model aggregates.
func (env *budgetTestEnv) seedKeySpend(t *testing.T, keyID string, costUSD float64, tokens int64) {
	t.Helper()
	ctx := context.Background()
	task := &models.Task{ID: uuid.New(), Prompt: "seeded spend", Status: models.TaskStatusSuccess,
		CreatedAt: env.now.Add(-2 * time.Hour), CreatedByKeyID: &keyID}
	if _, err := env.store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := env.store.AddTaskIteration(ctx, &models.TaskIteration{
		ID: uuid.New(), TaskID: task.ID, IterationNumber: 1, Status: "completed",
		StartedAt: env.now.Add(-time.Hour), CostUSD: costUSD, PromptTokens: tokens,
	}); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
}

func (env *budgetTestEnv) createKey(t *testing.T) (keyID, raw string) {
	t.Helper()
	key, raw, err := env.keyMgr.CreateKey("budgeted", nil, []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return key.KeyID, raw
}

func (env *budgetTestEnv) do(t *testing.T, method, path, body, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	env.r.ServeHTTP(w, req)
	return w
}

func (env *budgetTestEnv) putBudget(t *testing.T, body string) models.Budget {
	t.Helper()
	w := env.do(t, "POST", "/admin/budgets", body, "test-admin-key")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /admin/budgets: got %d (%s)", w.Code, w.Body.String())
	}
	var b models.Budget
	if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
		t.Fatalf("decode budget: %v", err)
	}
	return b
}

func TestBudgetCRUDEndpoints(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()

	// Non-admin callers are refused; malformed budgets are 400.
	if w := env.do(t, "GET", "/admin/budgets", "", ""); w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated list: got %d, want 403", w.Code)
	}
	for _, bad := range []string{
		`{"scope":"team","principal_id":"x","window":"day","hard_usd":1}`,
		`{"scope":"user","principal_id":"x","window":"hour","hard_usd":1}`,
		`{"scope":"user","principal_id":"","window":"day","hard_usd":1}`,
		`{"scope":"user","principal_id":"x","window":"day"}`,
		`{"scope":"user","principal_id":"x","window":"day","hard_usd":-1}`,
		`{"scope":"user","principal_id":"x","window":"day","soft_usd":5,"hard_usd":1}`,
		`{"scope":"user","principal_id":"x","window":"day","soft_tokens":50,"hard_tokens":10}`,
	} {
		if w := env.do(t, "POST", "/admin/budgets", bad, "test-admin-key"); w.Code != http.StatusBadRequest {
			t.Errorf("invalid budget %s: got %d, want 400 (%s)", bad, w.Code, w.Body.String())
		}
	}

	b := env.putBudget(t, `{"scope":"user","principal_id":"alice@example.com","window":"month","soft_usd":5,"hard_usd":10,"soft_tokens":1000,"hard_tokens":2000}`)
	if b.Scope != models.BudgetScopeUser || b.Window != models.BudgetWindowMonth || b.HardUSD == nil || *b.HardUSD != 10 {
		t.Fatalf("unexpected budget: %+v", b)
	}

	// The list surfaces live evaluation fields (zero spend so far).
	w := env.do(t, "GET", "/admin/budgets", "", "test-admin-key")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/budgets: got %d (%s)", w.Code, w.Body.String())
	}
	var listResp struct {
		Budgets []models.BudgetStatus `json:"budgets"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Budgets) != 1 {
		t.Fatalf("want 1 budget, got %d", len(listResp.Budgets))
	}
	s := listResp.Budgets[0]
	if !s.WindowStart.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window_start = %v, want 2026-07-01", s.WindowStart)
	}
	if s.SpendUSD != 0 || s.SpendTokens != 0 || s.SoftAlerted {
		t.Errorf("fresh budget should show zero spend and no alert: %+v", s)
	}

	if w := env.do(t, "DELETE", "/admin/budgets/"+b.ID.String(), "", "test-admin-key"); w.Code != http.StatusOK {
		t.Errorf("delete: got %d (%s)", w.Code, w.Body.String())
	}
	if w := env.do(t, "DELETE", "/admin/budgets/"+b.ID.String(), "", "test-admin-key"); w.Code != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want 404", w.Code)
	}
}

// TestBudgetHardRefusal_SingleAndBatch is the hard-bound acceptance criterion
// for the two HTTP create paths: once the key's window spend reaches hard_usd,
// POST /tasks and POST /tasks/batch refuse with 402 + Retry-After — and the
// window rollover lifts the refusal.
func TestBudgetHardRefusal_SingleAndBatch(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	keyID, raw := env.createKey(t)

	// Under the bound: creates pass.
	env.putBudget(t, `{"scope":"key","principal_id":"`+keyID+`","window":"day","hard_usd":10}`)
	env.seedKeySpend(t, keyID, 9.5, 100)
	if w := env.do(t, "POST", "/tasks", `{"prompt":"under the hard bound"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("under bound: got %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// Cross the bound and both paths refuse.
	env.seedKeySpend(t, keyID, 0.5, 100)
	w := env.do(t, "POST", "/tasks", `{"prompt":"over the hard bound"}`, raw)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("single create over bound: got %d, want 402 (%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("402 should carry Retry-After pointing at the window rollover")
	}
	wb := env.do(t, "POST", "/tasks/batch", `{"tasks":[{"prompt":"batched over the bound"}]}`, raw)
	if wb.Code != http.StatusPaymentRequired {
		t.Fatalf("batch create over bound: got %d, want 402 (%s)", wb.Code, wb.Body.String())
	}

	// The admin key carries no budget principal and is unaffected.
	if w := env.do(t, "POST", "/tasks", `{"prompt":"admin task unaffected"}`, "test-admin-key"); w.Code != http.StatusOK {
		t.Fatalf("admin create: got %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// Window rollover: the next UTC day the same key creates again.
	env.now = env.now.AddDate(0, 0, 1)
	if w := env.do(t, "POST", "/tasks", `{"prompt":"new window admits work"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("after rollover: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestBudgetTokenHardRefusal: budgets bound by TOKENS refuse even when dollar
// spend is zero (the #289 honest-scope case — native-provider runs accrue $0
// without a pricing override).
func TestBudgetTokenHardRefusal(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	keyID, raw := env.createKey(t)

	env.putBudget(t, `{"scope":"key","principal_id":"`+keyID+`","window":"week","hard_tokens":1000}`)
	env.seedKeySpend(t, keyID, 0, 1000)
	if w := env.do(t, "POST", "/tasks", `{"prompt":"token-bound refusal"}`, raw); w.Code != http.StatusPaymentRequired {
		t.Fatalf("token bound: got %d, want 402 (%s)", w.Code, w.Body.String())
	}
}

// TestBudgetSoftAlert_OncePerWindow_AcrossRestart: the soft crossing fires
// exactly ONE notify alert per window — repeat creates in the window stay
// silent, a process restart (fresh enforcer, same DB) cannot re-alert because
// the marker is persisted, and the next window re-arms.
func TestBudgetSoftAlert_OncePerWindow_AcrossRestart(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	keyID, raw := env.createKey(t)

	env.putBudget(t, `{"scope":"key","principal_id":"`+keyID+`","window":"day","soft_usd":5,"hard_usd":100}`)
	env.seedKeySpend(t, keyID, 6, 100)

	// First create over the soft bound: admitted + exactly one alert.
	if w := env.do(t, "POST", "/tasks", `{"prompt":"first soft crossing"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("soft crossing must admit: got %d (%s)", w.Code, w.Body.String())
	}
	if got := env.notifier.count(); got != 1 {
		t.Fatalf("after first crossing: %d alerts, want 1", got)
	}
	// Repeats in the same window: no duplicates.
	for i := 0; i < 3; i++ {
		if w := env.do(t, "POST", "/tasks", `{"prompt":"repeat create in window"}`, raw); w.Code != http.StatusOK {
			t.Fatalf("repeat create: got %d (%s)", w.Code, w.Body.String())
		}
	}
	if got := env.notifier.count(); got != 1 {
		t.Fatalf("after repeats: %d alerts, want still 1", got)
	}

	// "Restart": a brand-new enforcer over the same database must see the
	// persisted marker and stay silent.
	h2 := New(Config{DefaultTaskModel: "test/model", OrchestratorURL: "http://localhost:8000", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, env.store, env.keyMgr)
	h2.SetBudgetGate(env.newEnforcer())
	r2 := chi.NewRouter()
	r2.Post("/tasks", h2.CreateTask)
	req := httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(`{"prompt":"create after restart"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", raw)
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("create after restart: got %d (%s)", w2.Code, w2.Body.String())
	}
	if got := env.notifier.count(); got != 1 {
		t.Fatalf("after restart: %d alerts, want still 1 (marker is persisted)", got)
	}

	// Next window: spend seeded in the new day crosses again → second alert.
	env.now = env.now.AddDate(0, 0, 1)
	env.seedKeySpend(t, keyID, 6, 100)
	if w := env.do(t, "POST", "/tasks", `{"prompt":"crossing in the next window"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("next-window create: got %d (%s)", w.Code, w.Body.String())
	}
	if got := env.notifier.count(); got != 2 {
		t.Fatalf("after next-window crossing: %d alerts, want 2", got)
	}
}

// TestBudgetNeverExceedsGlobalCeiling: fail-safe composition with #286 — a
// budget hard bound ABOVE the live global ceiling is clamped down to it, and a
// live reload of the ceiling is honored on the very next create.
func TestBudgetNeverExceedsGlobalCeiling(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	keyID, raw := env.createKey(t)

	// Budget says $1000/day; the live global ceiling says $3. Spend $5.
	env.ceilUSD = 3
	env.putBudget(t, `{"scope":"key","principal_id":"`+keyID+`","window":"day","hard_usd":1000}`)
	env.seedKeySpend(t, keyID, 5, 100)
	if w := env.do(t, "POST", "/tasks", `{"prompt":"clamped by the global ceiling"}`, raw); w.Code != http.StatusPaymentRequired {
		t.Fatalf("budget above global ceiling: got %d, want 402 (%s)", w.Code, w.Body.String())
	}

	// Hot-reload raising the global (#286): read live, the budget's own bound
	// governs again and $5 < $1000 admits.
	env.ceilUSD = 5000
	if w := env.do(t, "POST", "/tasks", `{"prompt":"admitted after live ceiling raise"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("after live raise: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestNoBudgetBehaviorUnchanged: with the gate wired but no budget rows, every
// create path behaves exactly as before — no refusal, no alert.
func TestNoBudgetBehaviorUnchanged(t *testing.T) {
	env, cleanup := setupBudgetEnv(t)
	defer cleanup()
	keyID, raw := env.createKey(t)
	env.seedKeySpend(t, keyID, 12345, 99999999)

	if w := env.do(t, "POST", "/tasks", `{"prompt":"no budget, no gate"}`, raw); w.Code != http.StatusOK {
		t.Fatalf("single create: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if w := env.do(t, "POST", "/tasks/batch", `{"tasks":[{"prompt":"batch with no budget"}]}`, raw); w.Code != http.StatusOK {
		t.Fatalf("batch create: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := env.notifier.count(); got != 0 {
		t.Fatalf("no budget must never alert, got %d events", got)
	}
}
