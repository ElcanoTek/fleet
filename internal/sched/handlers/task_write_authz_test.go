// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Own-rows regression tests for three surfaces that authorized on a permission
// alone while the read path guarding the same rows had already been narrowed to
// own rows (#980/#1082):
//
//   - GET  /tasks/paused                    — returned every principal's rows
//   - POST /tasks/{id}/feedback             — wrote to any principal's task
//   - GET  /tasks/{id}/learned-instructions — read any principal's instructions
//
// PUT /tasks/{id} and POST /tasks/{id}/tags are covered by
// TestScopedAPIKeyAuthorization in principal_authz_test.go.

// taskAuthzRouter wires only the routes under test, behind the same middleware
// the server uses, so each request carries a real principal.
func taskAuthzRouter(t *testing.T) (*chi.Mux, *Handlers, func()) {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	h := New(Config{
		DefaultTaskModel: "test/model",
		OrchestratorURL:  "http://localhost:8000",
		AdminAPIKey:      "test-admin-key",
		Version:          "0.1.0",
	}, store, keyMgr)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Get("/tasks/paused", h.ListPausedTasks)
		r.Post("/tasks/{task_id}/feedback", h.SubmitFeedback)
		r.Get("/tasks/{task_id}/learned-instructions", h.LearnedInstructions)
	})
	return r, h, cleanup
}

func decodeTasks(t *testing.T, body []byte) []*models.Task {
	t.Helper()
	var got struct {
		Tasks []*models.Task `json:"tasks"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.Tasks
}

// A paused task carries its prompt in the projection, and ListPausedTasks
// selects on status alone with no principal predicate in SQL. Before the fix a
// client-role key read every principal's paused prompts — and their task UUIDs.
func TestListPausedTasksIsScopedToOwnRows(t *testing.T) {
	r, h, cleanup := taskAuthzRouter(t)
	defer cleanup()

	keyID, rawKey := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	mine := addTaskCreatedByKeyWithStatus(t, h.storage, "my own paused prompt", keyID, models.TaskStatusPausedAwaitingInput)

	otherKeyID, _ := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	theirs := addTaskCreatedByKeyWithStatus(t, h.storage, "SOMEONE ELSE secret prompt", otherKeyID, models.TaskStatusPausedAwaitingInput)

	req := httptest.NewRequest("GET", "/tasks/paused", nil)
	req.Header.Set("X-API-Key", rawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /tasks/paused = %d, want 200: %s", w.Code, w.Body.String())
	}

	tasks := decodeTasks(t, w.Body.Bytes())
	var sawMine bool
	for _, task := range tasks {
		if task.ID == theirs.ID {
			t.Fatalf("another principal's paused task leaked into the queue: %q", task.Prompt)
		}
		if task.ID == mine.ID {
			sawMine = true
		}
	}
	if !sawMine {
		t.Fatal("the principal's OWN paused task must still be listed — otherwise this asserts nothing")
	}
}

// An admin must still see the whole queue: a fleet-wide view is the point of the
// "needs a human answer" surface.
func TestListPausedTasksAdminSeesEveryRow(t *testing.T) {
	r, h, cleanup := taskAuthzRouter(t)
	defer cleanup()

	otherKeyID, _ := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	theirs := addTaskCreatedByKeyWithStatus(t, h.storage, "someone's paused prompt", otherKeyID, models.TaskStatusPausedAwaitingInput)

	req := httptest.NewRequest("GET", "/tasks/paused", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET /tasks/paused = %d, want 200: %s", w.Code, w.Body.String())
	}
	for _, task := range decodeTasks(t, w.Body.Bytes()) {
		if task.ID == theirs.ID {
			return
		}
	}
	t.Fatal("an admin must see other principals' paused tasks")
}

// Feedback writes to the task and can trigger LLM distillation against the
// victim's prompt, so it is own-rows. 404 rather than 403 so the surface does
// not confirm that an unowned task id exists.
func TestFeedbackAndLearnedInstructionsAreScopedToOwnRows(t *testing.T) {
	r, h, cleanup := taskAuthzRouter(t)
	defer cleanup()

	_, rawKey := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	otherKeyID, _ := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	theirs := addTaskCreatedByKey(t, h.storage, "someone else's prompt", otherKeyID)

	body, _ := json.Marshal(map[string]string{"rating": models.FeedbackDown, "critique": "attacker-authored critique"})
	req := httptest.NewRequest("POST", "/tasks/"+theirs.ID.String()+"/feedback", bytes.NewReader(body))
	req.Header.Set("X-API-Key", rawKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("feedback on an unowned task = %d, want 404: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/tasks/"+theirs.ID.String()+"/learned-instructions", nil)
	req.Header.Set("X-API-Key", rawKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("learned-instructions on an unowned task = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// The owning principal must still be able to use both surfaces.
func TestFeedbackOnOwnTaskIsAllowed(t *testing.T) {
	r, h, cleanup := taskAuthzRouter(t)
	defer cleanup()

	keyID, rawKey := mustCreateRoleKeyWithID(t, h.apiKeys, "client")
	mine := addTaskCreatedByKey(t, h.storage, "my own prompt", keyID)

	body, _ := json.Marshal(map[string]string{"rating": models.FeedbackUp})
	req := httptest.NewRequest("POST", "/tasks/"+mine.ID.String()+"/feedback", bytes.NewReader(body))
	req.Header.Set("X-API-Key", rawKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("feedback on own task = %d, want 200: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/tasks/"+mine.ID.String()+"/learned-instructions", nil)
	req.Header.Set("X-API-Key", rawKey)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("learned-instructions on own task = %d, want 200: %s", w.Code, w.Body.String())
	}
}
