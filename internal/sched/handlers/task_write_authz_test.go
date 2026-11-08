// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// wakeRouter wires the "steer a parked task" mutation behind the
// real middleware, plus one client-role session user so the owner-without-
// cancel-permission case is a real user principal (ownsTask resolves through
// the user id, never a key).
func wakeRouter(t *testing.T) (*chi.Mux, *Handlers, *models.User) {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Use(asSeededSession)
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Post("/tasks/{task_id}/wake", h.WakeTask)
	})
	owner := seedOpsUser(t, store, uuid.New(), "wake-owner", "client", "wake-owner-token")
	return r, h, owner
}

func addPausedTaskOwnedBy(t *testing.T, store interface {
	AddTask(*models.Task) (*models.Task, error)
}, ownerID *uuid.UUID, keyID *string, status models.TaskStatus) *models.Task {
	t.Helper()
	task := &models.Task{ID: uuid.New(), Prompt: "a parked task", Status: status, CreatedAt: time.Now().UTC(), CreatedBy: ownerID, CreatedByKeyID: keyID}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	return task
}

// POST /tasks/{id}/wake injects model input into a parked run (the wake note
// as event payload). The gate is CancelTask-shaped: cancel_task AND row
// visibility, OR the creating user.
func TestWakeIsScopedPerTask(t *testing.T) {
	t.Parallel()

	r, h, owner := wakeRouter(t)
	post := func(path, body string, auth func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		auth(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	withKey := func(raw string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set("X-API-Key", raw) }
	}
	asOwner := func(req *http.Request) { req.Header.Set("Authorization", "Bearer wake-owner-token") }

	// An operator key: cancel_task + view_tasks, but no fleet-wide grant.
	_, opRaw, err := h.apiKeys.CreateKey(context.Background(), "operator", []models.Permission{models.PermissionCancelTask, models.PermissionViewTasks}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	otherKeyID, _ := mustCreateRoleKeyWithID(t, h.apiKeys, "client")

	t.Run("cancel_task key may not wake a task it cannot see", func(t *testing.T) {
		parked := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingWake)
		if w := post("/tasks/"+parked.ID.String()+"/wake", `{"event":"deploy-finished"}`, withKey(opRaw)); w.Code != http.StatusForbidden {
			t.Fatalf("wake of an unseen task = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("the creating user may not wake another principal's task", func(t *testing.T) {
		theirs := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingWake)
		if w := post("/tasks/"+theirs.ID.String()+"/wake", `{"event":"deploy-finished"}`, asOwner); w.Code != http.StatusForbidden {
			t.Fatalf("client user waking another's task = %d, want 403: %s", w.Code, w.Body.String())
		}
		_ = owner
	})
}
