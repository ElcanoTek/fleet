// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

// resumeWakeRouter wires the two "steer a parked task" mutations behind the
// real middleware, plus one client-role session user so the owner-without-
// cancel-permission case is a real user principal (ownsTask resolves through
// the user id, never a key).
func resumeWakeRouter(t *testing.T) (*chi.Mux, *Handlers, *models.User) {
	t.Helper()
	store, keyMgr, _, cleanup := setupAuthzHandler(t)
	t.Cleanup(cleanup)
	h := New(Config{DefaultTaskModel: "test/model", OrchestratorURL: "http://localhost:8000", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(h.AdminOrUserAuthMiddleware)
		r.Post("/tasks/{task_id}/resume", h.ResumeTask)
		r.Post("/tasks/{task_id}/wake", h.WakeTask)
	})
	hash := models.HashToken("resume-owner-token")
	owner := &models.User{ID: uuid.New(), Username: "resume-owner", Role: "client", CreatedAt: time.Now(), SessionToken: &hash}
	if _, err := store.AddUser(owner); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
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

// POST /tasks/{id}/resume and /wake inject model input into a parked run (the
// answer verbatim; the wake note as event payload). They authorized on
// cancel_task alone, skipping the per-task check every sibling mutation
// performs, so a cancel_task key could steer ANY principal's task. The gate is
// now CancelTask-shaped: cancel_task AND row visibility, OR the creating user.
func TestResumeAndWakeAreScopedPerTask(t *testing.T) {
	r, h, owner := resumeWakeRouter(t)
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
	asOwner := func(req *http.Request) { req.Header.Set("Authorization", "Bearer resume-owner-token") }

	// An operator key: cancel_task + view_tasks, but no fleet-wide grant.
	opKey, opRaw, err := h.apiKeys.CreateKey("operator", []models.Permission{models.PermissionCancelTask, models.PermissionViewTasks}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	otherKeyID, _ := mustCreateRoleKeyWithID(t, h.apiKeys, "client")

	t.Run("cancel_task key may not resume or wake a task it cannot see", func(t *testing.T) {
		theirs := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingInput)
		if w := post("/tasks/"+theirs.ID.String()+"/resume", `{"answer":"attacker steering"}`, withKey(opRaw)); w.Code != http.StatusForbidden {
			t.Fatalf("resume of an unseen task = %d, want 403: %s", w.Code, w.Body.String())
		}
		parked := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingWake)
		if w := post("/tasks/"+parked.ID.String()+"/wake", `{"event":"deploy-finished"}`, withKey(opRaw)); w.Code != http.StatusForbidden {
			t.Fatalf("wake of an unseen task = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("cancel_task key may resume its own task", func(t *testing.T) {
		mine := addPausedTaskOwnedBy(t, h.storage, nil, &opKey.KeyID, models.TaskStatusPausedAwaitingInput)
		if w := post("/tasks/"+mine.ID.String()+"/resume", `{"answer":"yes, proceed"}`, withKey(opRaw)); w.Code != http.StatusOK {
			t.Fatalf("resume of own task = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("the creating user may resume without cancel permission", func(t *testing.T) {
		mine := addPausedTaskOwnedBy(t, h.storage, &owner.ID, nil, models.TaskStatusPausedAwaitingInput)
		if w := post("/tasks/"+mine.ID.String()+"/resume", `{"answer":"yes, proceed"}`, asOwner); w.Code != http.StatusOK {
			t.Fatalf("owner resume = %d, want 200: %s", w.Code, w.Body.String())
		}
		theirs := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingInput)
		if w := post("/tasks/"+theirs.ID.String()+"/resume", `{"answer":"not mine"}`, asOwner); w.Code != http.StatusForbidden {
			t.Fatalf("client user resuming another's task = %d, want 403: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin may resume any task", func(t *testing.T) {
		theirs := addPausedTaskOwnedBy(t, h.storage, nil, &otherKeyID, models.TaskStatusPausedAwaitingInput)
		if w := post("/tasks/"+theirs.ID.String()+"/resume", `{"answer":"operator answer"}`, withKey("test-admin-key")); w.Code != http.StatusOK {
			t.Fatalf("admin resume = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("an oversized answer is 400", func(t *testing.T) {
		mine := addPausedTaskOwnedBy(t, h.storage, &owner.ID, nil, models.TaskStatusPausedAwaitingInput)
		huge := `{"answer":"` + strings.Repeat("a", maxResumeAnswerChars+1) + `"}`
		if w := post("/tasks/"+mine.ID.String()+"/resume", huge, asOwner); w.Code != http.StatusBadRequest {
			t.Fatalf("oversized answer = %d, want 400: %s", w.Code, w.Body.String())
		}
	})
}
