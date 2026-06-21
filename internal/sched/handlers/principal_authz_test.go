// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

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
	if err := store.Initialize(filepath.Join(tmpDir, "test.db")); err != nil {
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
	for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM nodes", "DELETE FROM users"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}

	h := New(Config{
		OrchestratorURL:   "http://localhost:8000",
		AdminAPIKey:       "test-admin-key",
		RegistrationToken: "test-reg-token",
		Version:           "0.1.0",
	}, store, keyMgr)

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/nodes", h.ListNodes)
		r.Get("/nodes/{node_id}", h.GetNode)
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

func mustCreateScopedKey(t *testing.T, keyMgr *apikeys.Manager, role string, patterns []string) string {
	t.Helper()
	r := role
	_, raw, err := keyMgr.CreateKey("test-"+role, patterns, nil, &r, 0, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return raw
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
	roKey := mustCreateScopedKey(t, keyMgr, "readonly", []string{"client-a-*"})

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

	t.Run("client key can edit an editable task", func(t *testing.T) {
		// The client role carries create_task (which gates editing) but not
		// cancel_task, so editing is the right op to test mutating authorization
		// on a call a scoped key is actually permitted to make.
		clientKey := mustCreateScopedKey(t, keyMgr, "client", []string{"client-a-*"})

		body, _ := json.Marshal(models.TaskCreate{Prompt: "edited prompt that is sufficiently long"})
		req := httptest.NewRequest("PUT", "/tasks/"+taskA.ID.String(), bytes.NewReader(body))
		req.Header.Set("X-API-Key", clientKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("client key edit should be 200, got %d: %s", w.Code, w.Body.String())
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
