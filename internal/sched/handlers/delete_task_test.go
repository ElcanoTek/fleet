// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Cancelling keeps the row, and the row keeps its name — which carries a
// partial unique index (migration 036). So a job that broke could never be
// replaced under the same name, and could not be renamed either, because
// UpdateTask only edits pending or scheduled tasks. Deleting was the missing
// operation, and fleet had it nowhere.
func TestDeleteTaskFreesTheName(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	task := models.NewTask(models.TaskCreate{Name: "daily-refresh", Prompt: "refresh the dashboard"})
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	// Cancelling is NOT enough: the name is still taken afterwards.
	if _, err := store.CancelTaskAtomic(task.ID, "stopped"); err != nil {
		t.Fatalf("CancelTaskAtomic: %v", err)
	}
	replacement := models.NewTask(models.TaskCreate{Name: "daily-refresh", Prompt: "the fixed version"})
	if _, err := store.AddTask(replacement); err == nil {
		t.Fatal("a cancelled task must still hold its name — otherwise this test is asserting nothing")
	}

	rr := deleteTaskRequest(t, r, task.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE permanent = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if _, err := store.AddTask(replacement); err != nil {
		t.Fatalf("after delete, the name must be reusable: %v", err)
	}
	if got, _ := store.GetTask(task.ID); got != nil {
		t.Error("the deleted task row must be gone, not merely flagged")
	}
}

// A live run still holds its lease and is writing to the row. Pulling it out
// from under the worker is refused with an instruction, not a 500 later.
func TestDeleteTaskRefusesALiveRun(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	for _, status := range []models.TaskStatus{models.TaskStatusRunning, models.TaskStatusLeased} {
		task := models.NewTask(models.TaskCreate{Prompt: "busy"})
		task.Status = status
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		rr := deleteTaskRequest(t, r, task.ID)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s: DELETE = %d, want 409", status, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "Stop it first") {
			t.Errorf("%s: body = %q, want it to say what to do instead", status, rr.Body.String())
		}
		if got, _ := store.GetTask(task.ID); got == nil {
			t.Errorf("%s: a refused delete must leave the task alone", status)
		}
	}
}

// A terminal task is exactly the case this exists for: the broken job someone
// is trying to clear out of the way.
func TestDeleteTaskAcceptsTerminalStatuses(t *testing.T) {
	r, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	for _, status := range []models.TaskStatus{
		models.TaskStatusSuccess, models.TaskStatusError,
		models.TaskStatusCancelled, models.TaskStatusDeadLettered,
		models.TaskStatusPending, models.TaskStatusScheduled,
	} {
		task := models.NewTask(models.TaskCreate{Prompt: string(status)})
		task.Status = status
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		if rr := deleteTaskRequest(t, r, task.ID); rr.Code != http.StatusOK {
			t.Errorf("%s: DELETE = %d, want 200: %s", status, rr.Code, rr.Body.String())
		}
	}
}

// Unknown ids and double-clicks report "gone", not a server error.
func TestDeleteTaskMissing(t *testing.T) {
	r, cleanup := setupTestHandler(t)
	defer cleanup()
	if rr := deleteTaskRequest(t, r, uuid.New()); rr.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown id = %d, want 404", rr.Code)
	}
}

func deleteTaskRequest(t *testing.T, r *chi.Mux, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/tasks/"+id.String()+"/permanent", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}
