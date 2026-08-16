// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Regression tests for #1082: a task row is visible to the principal that
// created it, or to a principal holding a fleet-wide grant (admin /
// view_all_logs) — and to nobody else. Before the fix, any view_tasks holder
// (including a scoped fleet_task_* key minted for one automation) could list
// and read every task on the box.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupTaskAuthz wires the task read routes behind the real
// AdminOrUserAuthMiddleware so each credential class travels the whole path
// (middleware admits, handler authorizes).
func setupTaskAuthz(t *testing.T) (*storage.Storage, *apikeys.Manager, *chi.Mux) {
	t.Helper()
	tmpDir := t.TempDir()

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Skipping tests: database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	acquireTestLock(t, store)
	if err := cleanDB(store); err != nil {
		t.Fatalf("clean db: %v", err)
	}

	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "keys.json"), filepath.Join(tmpDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("key mgr: %v", err)
	}

	h := New(Config{
		DefaultTaskModel: "test/model",
		AdminAPIKey:      "admin-key",
		DataDir:          tmpDir,
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks", h.ListTasks)
		r.Get("/tasks/export", h.HandleTaskExport)
		r.Get("/tasks/upcoming", h.GetUpcomingRuns)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Get("/tasks/{task_id}/output", h.GetTaskOutput)
		r.Get("/tasks/{task_id}/error-analysis", h.GetTaskErrorAnalysis)
		r.Post("/tasks/{task_id}/rerun", h.RerunTask)
	})
	return store, keyMgr, r
}

// addOwnedTask creates a task attributed to the given creator (user and/or API
// key, either may be nil) whose prompt must not leak to other principals.
func addOwnedTask(t *testing.T, store *storage.Storage, createdBy *uuid.UUID, createdByKeyID *string) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         "task with a sensitive prompt",
		Status:         models.TaskStatusSuccess,
		CreatedBy:      createdBy,
		CreatedByKeyID: createdByKeyID,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task
}

// listTaskIDs performs GET /tasks with the given credential and returns the ids
// in the page (the visibility filter runs in SQL, so Total reflects it too).
func listTaskIDs(t *testing.T, r *chi.Mux, header, value string) map[string]bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set(header, value)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /tasks: code=%d", w.Code)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if resp.Total != len(resp.Data) {
		t.Errorf("list total=%d disagrees with page len=%d (visibility must filter the count too)", resp.Total, len(resp.Data))
	}
	ids := make(map[string]bool, len(resp.Data))
	for _, d := range resp.Data {
		ids[d.ID] = true
	}
	return ids
}

// TestTaskReadIsCreatorScoped is the #1082 regression: a fleet_task_ key sees
// only its own rows on list, get, and the get-shaped sub-reads, while the
// creating key still reads its own.
func TestTaskReadIsCreatorScoped(t *testing.T) {
	store, keyMgr, r := setupTaskAuthz(t)

	ownerKey, ownerRaw, err := keyMgr.CreateTypedKey("owner", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	_, intruderRaw, err := keyMgr.CreateTypedKey("intruder", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create intruder key: %v", err)
	}

	owned := addOwnedTask(t, store, nil, &ownerKey.KeyID)

	// List: the owner sees its row, the intruder sees an empty fleet.
	if ids := listTaskIDs(t, r, "X-API-Key", ownerRaw); !ids[owned.ID.String()] {
		t.Error("creating key must list its own task")
	}
	if ids := listTaskIDs(t, r, "X-API-Key", intruderRaw); len(ids) != 0 {
		t.Errorf("intruder key must list nothing, got %d rows", len(ids))
	}

	// Get and the get-shaped sub-reads: 403 for the intruder, not the row.
	for _, path := range []string{
		"/tasks/" + owned.ID.String(),
		"/tasks/" + owned.ID.String() + "/output",
		"/tasks/" + owned.ID.String() + "/error-analysis",
	} {
		if code := logRequest(t, r, path, "X-API-Key", intruderRaw); code != http.StatusForbidden {
			t.Errorf("intruder on %s: want 403, got %d", path, code)
		}
	}
	if code := logRequest(t, r, "/tasks/"+owned.ID.String(), "X-API-Key", ownerRaw); code != http.StatusOK {
		t.Errorf("creating key must read its own task: got %d", code)
	}

	// Rerun would copy the source's prompt into a row the intruder owns.
	req := httptest.NewRequest(http.MethodPost, "/tasks/"+owned.ID.String()+"/rerun", nil)
	req.Header.Set("X-API-Key", intruderRaw)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("intruder rerun: want 403, got %d", w.Code)
	}

	// Export is list-shaped: the intruder's bundle must not carry the task.
	req = httptest.NewRequest(http.MethodGet, "/tasks/export", nil)
	req.Header.Set("X-API-Key", intruderRaw)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("intruder export: code=%d", w.Code)
	}
	var envelope struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(envelope.Tasks) != 0 {
		t.Errorf("intruder export must be empty, got %d records", len(envelope.Tasks))
	}
}

// TestTaskReadFleetWideGrants pins who still sees every task: the bootstrap
// admin key, a typed admin key, and a key minted with the explicit
// view_all_logs auditor grant (an auditor who may read every transcript must be
// able to find the tasks those transcripts belong to).
func TestTaskReadFleetWideGrants(t *testing.T) {
	store, keyMgr, r := setupTaskAuthz(t)

	task := addOwnedTask(t, store, nil, nil) // created out-of-band; owned by nobody
	path := "/tasks/" + task.ID.String()

	if code := logRequest(t, r, path, "X-API-Key", "admin-key"); code != http.StatusOK {
		t.Errorf("bootstrap admin key must read any task: got %d", code)
	}
	if ids := listTaskIDs(t, r, "X-API-Key", "admin-key"); !ids[task.ID.String()] {
		t.Error("bootstrap admin key must list every task")
	}

	_, typedAdminRaw, err := keyMgr.CreateTypedKey("root", apikeys.KeyTypeAdmin, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create typed admin key: %v", err)
	}
	if code := logRequest(t, r, path, "X-API-Key", typedAdminRaw); code != http.StatusOK {
		t.Errorf("typed admin key must read any task: got %d", code)
	}

	_, auditorRaw, err := keyMgr.CreateKey("auditor", []models.Permission{models.PermissionViewTasks, models.PermissionViewLogs, models.PermissionViewAllLogs},
		nil, 0, nil, "fleet-wide auditor")
	if err != nil {
		t.Fatalf("create auditor key: %v", err)
	}
	if code := logRequest(t, r, path, "X-API-Key", auditorRaw); code != http.StatusOK {
		t.Errorf("view_all_logs key must read any task: got %d", code)
	}

	// An unattributed task is visible only under a fleet-wide grant.
	_, plainRaw, err := keyMgr.CreateTypedKey("plain", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create plain key: %v", err)
	}
	if code := logRequest(t, r, path, "X-API-Key", plainRaw); code != http.StatusForbidden {
		t.Errorf("plain task key must not read an unowned task: got %d", code)
	}
}

// TestTaskReadUserScoping covers the user (web UI) credential class: a
// non-admin user sees its own tasks only — matching the transcript model (#980)
// — while an admin-role user sees the fleet.
func TestTaskReadUserScoping(t *testing.T) {
	store, _, r := setupTaskAuthz(t)

	alice := addLogUser(t, store, "alice", "client", "alice-token")
	addLogUser(t, store, "bob", "client", "bob-token")
	addLogUser(t, store, "root", "admin", "root-token")

	task := addOwnedTask(t, store, &alice.ID, nil)
	path := "/tasks/" + task.ID.String()

	if code := logRequest(t, r, path, "Authorization", "Bearer alice-token"); code != http.StatusOK {
		t.Errorf("creator must read own task: got %d", code)
	}
	if code := logRequest(t, r, path, "Authorization", "Bearer bob-token"); code != http.StatusForbidden {
		t.Errorf("non-creator user must be refused: got %d", code)
	}
	if code := logRequest(t, r, path, "Authorization", "Bearer root-token"); code != http.StatusOK {
		t.Errorf("admin user must read any task: got %d", code)
	}

	if ids := listTaskIDs(t, r, "Authorization", "Bearer bob-token"); len(ids) != 0 {
		t.Errorf("non-creator user must list nothing, got %d rows", len(ids))
	}
	if ids := listTaskIDs(t, r, "Authorization", "Bearer root-token"); !ids[task.ID.String()] {
		t.Error("admin user must list every task")
	}
}
