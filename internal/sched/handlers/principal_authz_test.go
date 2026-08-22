// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// setupAuthzHandler wires a router with the real AdminOrUserAuthMiddleware so
// the scoped-API-key authorization path (middleware admits the key, handlers
// enforce permission) is exercised end to end.
func setupAuthzHandler(t *testing.T) (*storage.Storage, *apikeys.Manager, *chi.Mux, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "sched-authz-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}

	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		os.RemoveAll(tmpDir)
		if isDatabaseUnavailable(err) {
			t.Skipf("database unavailable: %v", err)
		}
		t.Fatalf("init storage: %v", err)
	}
	acquireTestLock(t, store)

	keyMgr, err := apikeys.NewManager(
		filepath.Join(tmpDir, "api_keys.json"),
		filepath.Join(tmpDir, "audit_log.jsonl"),
	)
	if err != nil {
		store.Close()
		os.RemoveAll(tmpDir)
		t.Fatalf("key mgr: %v", err)
	}

	ctx := context.Background()
	for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}

	h := New(Config{DefaultTaskModel: "test/model",
		OrchestratorURL: "http://localhost:8000",
		AdminAPIKey:     "test-admin-key",
		Version:         "0.1.0",
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks", h.ListTasks)
		r.Get("/tasks/{task_id}", h.GetTask)
		r.Put("/tasks/{task_id}", h.UpdateTask)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		r.Get("/logs/{task_id}", h.GetLogs)
	})

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}
	return store, keyMgr, r, cleanup
}

func mustCreateRoleKey(t *testing.T, keyMgr *apikeys.Manager, role string) string {
	t.Helper()
	_, raw := mustCreateRoleKeyWithID(t, keyMgr, role)
	return raw
}

// mustCreateRoleKeyWithID also returns the key's KeyID, so a test can attribute
// a task to the key (task.CreatedByKeyID) and exercise own-rows authorization
// on an API-key principal rather than only on a user principal.
func mustCreateRoleKeyWithID(t *testing.T, keyMgr *apikeys.Manager, role string) (string, string) {
	t.Helper()
	r := role
	key, raw, err := keyMgr.CreateKey("test-"+role+"-"+uuid.NewString(), nil, &r, 0, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return key.KeyID, raw
}

// addTaskCreatedByKey inserts a task attributed to the given API key. The
// column is written on insert (taskColumnRegistry), so it is set before AddTask.
func addTaskCreatedByKey(t *testing.T, store *storage.Storage, prompt, keyID string) *models.Task {
	t.Helper()
	return addTaskCreatedByKeyWithStatus(t, store, prompt, keyID, models.TaskStatusPending)
}

// addTaskCreatedByKeyWithStatus is the same, at a chosen status — the paused
// queue selects on status alone, so its tests need rows already paused.
func addTaskCreatedByKeyWithStatus(t *testing.T, store *storage.Storage, prompt, keyID string, status models.TaskStatus) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:             uuid.New(),
		Prompt:         prompt,
		Status:         status,
		CreatedAt:      time.Now().UTC(),
		CreatedByKeyID: &keyID,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task
}

func addTask(t *testing.T, store *storage.Storage, prompt string) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    prompt,
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task
}

func TestPrincipalOwnsTask(t *testing.T) {
	aliceID, bobID := uuid.New(), uuid.New()
	alice := principal{user: &models.User{ID: aliceID}}
	if !alice.ownsTask(&models.Task{CreatedBy: &aliceID}) {
		t.Fatal("creator should own their task")
	}
	if alice.ownsTask(&models.Task{CreatedBy: &bobID}) || alice.ownsTask(&models.Task{}) {
		t.Fatal("user must not own another user's or unattributed task")
	}
	if (principal{apiKey: &apikeys.APIKey{KeyID: "key-1"}}).ownsTask(&models.Task{CreatedBy: &aliceID}) {
		t.Fatal("API keys must use explicit cancel_task permission")
	}
}

// TestScopedAPIKeyAuthorization is the regression test for the authorization
// bypass where a scoped API key admitted by AdminOrUserAuthMiddleware skipped
// every handler's permission check (those were gated on user != nil).
//
// Node-target scope visibility was removed with the move to per-task
// mcp_selection (tasks no longer carry a node target), so this retains only the
// permission-based authorization assertions (a readonly key cannot mutate; a
// client-role key can edit an editable task).
func TestScopedAPIKeyAuthorization(t *testing.T) {
	store, keyMgr, r, cleanup := setupAuthzHandler(t)
	defer cleanup()

	taskA := addTask(t, store, "task A")

	// A readonly key scoped to client-a-* carries view permissions but no
	// mutating ones.
	roKey := mustCreateRoleKey(t, keyMgr, "readonly")

	t.Run("readonly key cannot cancel any task", func(t *testing.T) {
		req := httptest.NewRequest("DELETE", "/tasks/"+taskA.ID.String(), nil)
		req.Header.Set("X-API-Key", roKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("readonly key must not cancel tasks; got %d", w.Code)
		}
	})

	t.Run("readonly key cannot edit any task", func(t *testing.T) {
		body, _ := json.Marshal(models.TaskCreate{Prompt: "hijacked prompt that is long enough"})
		req := httptest.NewRequest("PUT", "/tasks/"+taskA.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", roKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("readonly key must not edit tasks; got %d", w.Code)
		}
	})

	// Editing is own-rows, not merely permission-gated (taskWritableByPrincipal).
	// The client role carries create_task (which admits it to the edit surface)
	// but not cancel_task, so it is the right role to test WHICH task a scoped
	// key may mutate.
	t.Run("client key can edit a task it created", func(t *testing.T) {
		keyID, clientKey := mustCreateRoleKeyWithID(t, keyMgr, "client")

		// Attributed to this key — the scoped-intake-app case that must keep working.
		own := addTaskCreatedByKey(t, store, "a task this key created", keyID)

		body, _ := json.Marshal(models.TaskCreate{Prompt: "edited prompt that is sufficiently long"})
		req := httptest.NewRequest("PUT", "/tasks/"+own.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", clientKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("client key editing its OWN task should be 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	// The regression this pair exists for: PUT /tasks/{id} authorized on
	// PermissionCreateTask alone, so a scoped key could rewrite a task it did
	// not create — prompt, model, mcp_selection, credential_allowlist — while
	// the READ path for the same row was already narrowed to own rows (#1082).
	t.Run("client key cannot edit a task it did not create", func(t *testing.T) {
		clientKey := mustCreateRoleKey(t, keyMgr, "client")

		body, _ := json.Marshal(models.TaskCreate{Prompt: "hijacked prompt that is long enough"})
		req := httptest.NewRequest("PUT", "/tasks/"+taskA.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", clientKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("client key must not edit an unowned task; got %d: %s", w.Code, w.Body.String())
		}
		after, _ := store.GetTask(taskA.ID)
		if after == nil || after.Prompt != "task A" {
			t.Fatalf("a refused edit must leave the prompt alone, got %q", after.Prompt)
		}
	})
}

// TestUpdateTaskNotEditableConflict verifies the transactional UpdateTask
// rejects edits to a task that has left the editable state, instead of blindly
// overwriting it (which previously could resurrect a leased/cancelled task).
func TestUpdateTaskNotEditableConflict(t *testing.T) {
	store, _, r, cleanup := setupAuthzHandler(t)
	defer cleanup()

	// A running task is no longer editable.
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    "running task",
		Status:    models.TaskStatusRunning,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	body, _ := json.Marshal(models.TaskCreate{Prompt: "trying to edit a running task here"})
	req := httptest.NewRequest("PUT", "/tasks/"+task.ID.String(), bytes.NewReader(body))
	req.Header.Set("X-API-Key", "test-admin-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// The handler's pre-check returns 400 for a clearly-running task; the 409
	// path covers the race where status changes after the read. Either way it
	// must be rejected, never 200.
	if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
		t.Fatalf("editing a running task must be rejected, got %d: %s", w.Code, w.Body.String())
	}
}
