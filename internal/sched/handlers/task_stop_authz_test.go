// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskStopFollowsTaskScope pins taskStoppableByPrincipal on both stop
// surfaces, DELETE /tasks/{id} (cancel) and DELETE /tasks/{id}/permanent:
// the cancel permission reaches the tasks the principal can see, the creator
// (a user or an API key) may always stop its own task, and admin and
// view_all_logs principals keep fleet-wide reach.
func TestTaskStopFollowsTaskScope(t *testing.T) {
	t.Parallel()

	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	defer cleanup()
	h := New(Config{SharedToken: testSharedToken, DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Delete("/tasks/{task_id}", h.CancelTask)
		r.Delete("/tasks/{task_id}/permanent", h.DeleteTask)
	})
	do := func(path, key string) int {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	status := func(task *models.Task) models.TaskStatus {
		t.Helper()
		got, err := store.GetTask(task.ID)
		if err != nil || got == nil {
			t.Fatalf("GetTask %s: %v", task.ID, err)
		}
		return got.Status
	}

	_, operatorKey, err := keyMgr.CreateKey("operator", []models.Permission{models.PermissionCancelTask, models.PermissionViewTasks}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create operator key: %v", err)
	}
	_, auditorOperatorKey, err := keyMgr.CreateKey("auditor-operator",
		[]models.Permission{models.PermissionCancelTask, models.PermissionViewTasks, models.PermissionViewAllLogs}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create auditor-operator key: %v", err)
	}
	taskKey, taskKeyRaw, err := keyMgr.CreateTypedKey("automation", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("create task key: %v", err)
	}

	t.Run("cancel permission without the fleet-wide grant does not reach another principal's task", func(t *testing.T) {
		foreign := addTask(t, store, "someone else's task")
		if code := do("/tasks/"+foreign.ID.String(), operatorKey); code != http.StatusNotFound {
			t.Errorf("cancel = %d, want 404", code)
		}
		if code := do("/tasks/"+foreign.ID.String()+"/permanent", operatorKey); code != http.StatusNotFound {
			t.Errorf("permanent delete = %d, want 404", code)
		}
		if got := status(foreign); got != models.TaskStatusPending {
			t.Errorf("task status after refused stops = %s, want pending", got)
		}
	})

	t.Run("a typed task key stops and deletes its own task", func(t *testing.T) {
		own := addTaskCreatedByKey(t, store, "a task this key created", taskKey.KeyID)
		if code := do("/tasks/"+own.ID.String(), taskKeyRaw); code != http.StatusOK {
			t.Fatalf("cancel own task = %d, want 200", code)
		}
		if got := status(own); got != models.TaskStatusCancelled {
			t.Errorf("own task status = %s, want cancelled", got)
		}
		if code := do("/tasks/"+own.ID.String()+"/permanent", taskKeyRaw); code != http.StatusOK {
			t.Fatalf("delete own task = %d, want 200", code)
		}
	})

	t.Run("a typed task key cannot stop another key's task", func(t *testing.T) {
		otherKeyID, _ := mustCreateRoleKeyWithID(t, keyMgr, "client")
		other := addTaskCreatedByKey(t, store, "another key's task", otherKeyID)
		if code := do("/tasks/"+other.ID.String(), taskKeyRaw); code != http.StatusNotFound {
			t.Errorf("cancel = %d, want 404", code)
		}
	})

	t.Run("view_all_logs with the cancel permission stops any task", func(t *testing.T) {
		foreign := addTask(t, store, "fleet task")
		if code := do("/tasks/"+foreign.ID.String(), auditorOperatorKey); code != http.StatusOK {
			t.Errorf("cancel = %d, want 200", code)
		}
	})

	t.Run("admin stops and deletes any task", func(t *testing.T) {
		foreign := addTask(t, store, "fleet task for admin")
		if code := do("/tasks/"+foreign.ID.String(), "test-admin-key"); code != http.StatusOK {
			t.Errorf("admin cancel = %d, want 200", code)
		}
		if code := do("/tasks/"+foreign.ID.String()+"/permanent", "test-admin-key"); code != http.StatusOK {
			t.Errorf("admin delete = %d, want 200", code)
		}
	})
}
