package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// setupPriorityHandler builds a handler wired with the routes the task-priority
// tests need: POST /tasks (its own auth), GET /admin/queue (admin-gated), and a
// returned key manager so a test can mint a scoped key with a priority ceiling
// (#230).
func setupPriorityHandler(t *testing.T) (*chi.Mux, *storage.Storage, *apikeys.Manager, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sched-prio-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	store := storage.New()
	if err := store.Initialize(fleetdb.New(t).SchedDSN(), fleetdb.SchedPool()); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("storage init: %v", err)
	}

	keyMgr, err := apikeys.NewManager(
		filepath.Join(tmpDir, "api_keys.json"),
		filepath.Join(tmpDir, "audit_log.jsonl"),
	)
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("key manager: %v", err)
	}

	h := New(Config{DefaultTaskModel: "test/model",
		AdminAPIKey: "test-admin-key",
		Version:     "0.1.0",
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminAuthMiddleware)
		r.Get("/admin/queue", h.QueueStats)
	})
	r.Post("/tasks", h.CreateTask)
	r.Post("/tasks/batch", h.CreateTaskBatch)
	r.With(h.AdminOrUserAuthMiddleware).Put("/tasks/{task_id}", h.UpdateTask)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return r, store, keyMgr, cleanup
}

func postTask(t *testing.T, r *chi.Mux, body, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/tasks", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestCreateTask_DefaultPriority: an unset priority defaults to Normal (50) on
// both the submitted and effective columns (#230).
func TestCreateTask_DefaultPriority(t *testing.T) {
	t.Parallel()

	r, _, _, cleanup := setupPriorityHandler(t)
	defer cleanup()

	w := postTask(t, r, `{"prompt":"a task with no priority set"}`, "test-admin-key")
	if w.Code != http.StatusOK {
		t.Fatalf("create: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var task models.Task
	if err := json.NewDecoder(w.Body).Decode(&task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if task.Priority != models.PriorityNormal || task.EffectivePriority != models.PriorityNormal {
		t.Errorf("unset priority = (%d,%d), want (%d,%d)", task.Priority, task.EffectivePriority, models.PriorityNormal, models.PriorityNormal)
	}
}

// TestCreateTask_PriorityOutOfRange rejects a priority outside [0,100] (#230).
func TestCreateTask_PriorityOutOfRange(t *testing.T) {
	t.Parallel()

	r, _, _, cleanup := setupPriorityHandler(t)
	defer cleanup()

	for _, body := range []string{`{"prompt":"too urgent please","priority":-1}`, `{"prompt":"way too low please","priority":200}`} {
		if w := postTask(t, r, body, "test-admin-key"); w.Code != http.StatusBadRequest {
			t.Errorf("out-of-range %s: got %d, want 400", body, w.Code)
		}
	}
}

// TestCreateTask_KeyPriorityCeiling is acceptance criterion #5 (#230): a key
// capped at max_priority=40 may submit priority 50 (less urgent) but is refused
// for priority 10 (more urgent than its ceiling).
func TestCreateTask_KeyPriorityCeiling(t *testing.T) {
	t.Parallel()

	r, _, keyMgr, cleanup := setupPriorityHandler(t)
	defer cleanup()

	key, raw, err := keyMgr.CreateKey(context.Background(), "capped", []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	forty := 40
	if err := keyMgr.SetMaxPriority(key.KeyID, &forty); err != nil {
		t.Fatalf("SetMaxPriority: %v", err)
	}

	// Within the ceiling (50 is less urgent than 40) → allowed.
	if w := postTask(t, r, `{"prompt":"a normal-urgency task","priority":50}`, raw); w.Code != http.StatusOK {
		t.Fatalf("priority 50 under cap 40: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// More urgent than the ceiling (10 < 40) → forbidden.
	if w := postTask(t, r, `{"prompt":"a too-urgent task","priority":10}`, raw); w.Code != http.StatusForbidden {
		t.Errorf("priority 10 over cap 40: got %d, want 403", w.Code)
	}
}

// TestCreateTaskBatch_KeyPriorityCeiling guards against the batch path being a
// weaker route around the per-key ceiling (#230): a key capped at 40 has its
// over-cap entry rejected, and in atomic mode that aborts the whole batch.
func TestCreateTaskBatch_KeyPriorityCeiling(t *testing.T) {
	t.Parallel()

	r, store, keyMgr, cleanup := setupPriorityHandler(t)
	defer cleanup()

	key, raw, err := keyMgr.CreateKey(context.Background(), "capped", []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	forty := 40
	if err := keyMgr.SetMaxPriority(key.KeyID, &forty); err != nil {
		t.Fatalf("SetMaxPriority: %v", err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/tasks/batch", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", raw)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Atomic batch with one over-cap entry (priority 10 < 40) → 422, nothing created.
	body := `{"atomic":true,"tasks":[{"prompt":"a normal batch task body","priority":50},{"prompt":"a too-urgent batch task body","priority":10}]}`
	if w := post(body); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("atomic over-cap batch: got %d, want 422 (%s)", w.Code, w.Body.String())
	}
	if _, total, _ := store.GetTasksFiltered(db.TaskFilter{}, 1, 0); total != 0 {
		t.Errorf("atomic batch must create nothing on cap violation, found %d tasks", total)
	}

	// A batch entirely within the ceiling (priority 50) succeeds.
	if w := post(`{"atomic":true,"tasks":[{"prompt":"a within-cap batch task body","priority":50}]}`); w.Code != http.StatusOK {
		t.Errorf("within-cap batch: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestQueueStats: GET /admin/queue rolls pending tasks into named tiers with
// per-tier counts and a total (#230).
func TestQueueStats(t *testing.T) {
	t.Parallel()

	r, store, _, cleanup := setupPriorityHandler(t)
	defer cleanup()

	// Two critical, one bulk, all pending (NewTask sets effective = priority).
	for _, p := range []int{models.PriorityCritical, models.PriorityCritical, models.PriorityBulk} {
		if _, err := store.AddTask(models.NewTask(models.TaskCreate{Prompt: "queued task body", Priority: p})); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/admin/queue", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("queue stats: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var stats models.QueueStats
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.PendingTotal != 3 {
		t.Errorf("pending_total = %d, want 3", stats.PendingTotal)
	}
	byTier := map[string]int{}
	for _, ti := range stats.Tiers {
		byTier[ti.Tier] = ti.Count
	}
	if byTier["critical"] != 2 || byTier["bulk"] != 1 {
		t.Errorf("tier counts wrong: critical=%d bulk=%d (%+v)", byTier["critical"], byTier["bulk"], stats.Tiers)
	}
}

// TestUpdateTask_PreservesPriorityAndMaxTotalTokensWhenOmitted verifies that
// PUT /tasks/{id} preserves a task's priority and max_total_tokens when the
// request body omits them (such as an orchestrator UI edit), while updating
// them when explicitly supplied and enforcing the API key priority ceiling.
func TestUpdateTask_PreservesPriorityAndMaxTotalTokensWhenOmitted(t *testing.T) {
	t.Parallel()

	r, _, keyMgr, cleanup := setupPriorityHandler(t)
	defer cleanup()

	key, raw, err := keyMgr.CreateKey(context.Background(), "editor", []models.Permission{models.PermissionCreateTask, models.PermissionViewTasks}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	forty := 40
	if err := keyMgr.SetMaxPriority(key.KeyID, &forty); err != nil {
		t.Fatalf("SetMaxPriority: %v", err)
	}

	w := postTask(t, r, `{"prompt":"campaign-health scan","priority":75,"max_total_tokens":250000}`, raw)
	if w.Code != http.StatusOK {
		t.Fatalf("create: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var created models.Task
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/tasks/"+created.ID.String(), bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", raw)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// Edit without priority or max_total_tokens → both stay intact (never reset to 0 / nil).
	w = put(`{"prompt":"campaign-health scan v2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update omitted: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var updated models.Task
	if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.Priority != 75 || updated.EffectivePriority != 75 {
		t.Fatalf("priority after omitted edit = (%d,%d), want (75,75)", updated.Priority, updated.EffectivePriority)
	}
	if updated.MaxTotalTokens == nil || *updated.MaxTotalTokens != 250000 {
		t.Fatalf("max_total_tokens after omitted edit = %v, want 250000", updated.MaxTotalTokens)
	}

	// Explicit priority and max_total_tokens within key ceiling → updated.
	w = put(`{"prompt":"campaign-health scan v3","priority":60,"max_total_tokens":500000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update explicit: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var updatedExplicit models.Task
	if err := json.NewDecoder(w.Body).Decode(&updatedExplicit); err != nil {
		t.Fatalf("decode explicit update: %v", err)
	}
	if updatedExplicit.Priority != 60 || updatedExplicit.EffectivePriority != 60 {
		t.Fatalf("priority after explicit edit = (%d,%d), want (60,60)", updatedExplicit.Priority, updatedExplicit.EffectivePriority)
	}
	if updatedExplicit.MaxTotalTokens == nil || *updatedExplicit.MaxTotalTokens != 500000 {
		t.Fatalf("max_total_tokens after explicit edit = %v, want 500000", updatedExplicit.MaxTotalTokens)
	}

	// Explicit null clears max_total_tokens back to nil (inherit deployment ceiling),
	// and explicit priority: 0 resets priority to Normal (50).
	w = put(`{"prompt":"campaign-health scan v3b","priority":0,"max_total_tokens":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update explicit null max_total_tokens and priority 0: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var updatedCleared models.Task
	if err := json.NewDecoder(w.Body).Decode(&updatedCleared); err != nil {
		t.Fatalf("decode cleared update: %v", err)
	}
	if updatedCleared.MaxTotalTokens != nil {
		t.Fatalf("max_total_tokens after explicit null = %v, want nil", updatedCleared.MaxTotalTokens)
	}
	if updatedCleared.Priority != 50 || updatedCleared.EffectivePriority != 50 {
		t.Fatalf("priority after explicit 0 = (%d,%d), want (50,50)", updatedCleared.Priority, updatedCleared.EffectivePriority)
	}

	// Editing priority above the key's ceiling (10 < 40) → 403 Forbidden.
	if w := put(`{"prompt":"campaign-health scan v4","priority":10}`); w.Code != http.StatusForbidden {
		t.Fatalf("priority 10 over cap 40 on edit: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
}
