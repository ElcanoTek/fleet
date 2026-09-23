package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The task form now sends its Repeat time zone on every save, so moving an
// existing task off UTC is an edit that changes only `timezone`. That must
// move the next run too: "0 8 * * 1-5" re-evaluated in America/New_York, not
// left at the instant computed in the old zone (which would fire once at the
// stale time). The edit path gets this from validateTaskCreate deriving
// scheduled_for from the cron in the request's zone; this pins it.
func TestUpdateTask_TimezoneChangeMovesNextRun(t *testing.T) {
	store, h, cleanup := setupFullHandler(t)
	defer cleanup()
	r := chi.NewRouter()
	r.Group(func(rr chi.Router) {
		rr.Use(h.AdminOrUserAuthMiddleware)
		rr.Put("/tasks/{task_id}", h.UpdateTask)
	})
	put := func(id uuid.UUID, body models.TaskCreate) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPut, "/tasks/"+id.String(), bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	const cronExpr = "0 8 * * 1-5"
	next := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Minute)
	task := &models.Task{ID: uuid.New(), Prompt: "weekday morning report", Status: models.TaskStatusScheduled,
		CreatedAt: time.Now().UTC(), ScheduledFor: &next, Recurrence: cronExpr, Timezone: "UTC"}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := put(task.ID, models.TaskCreate{Prompt: task.Prompt, Recurrence: cronExpr, Timezone: "America/New_York"})
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Timezone != "America/New_York" {
		t.Fatalf("Timezone = %q, want America/New_York", got.Timezone)
	}
	if got.ScheduledFor == nil {
		t.Fatal("ScheduledFor cleared")
	}
	ny, _ := time.LoadLocation("America/New_York")
	local := got.ScheduledFor.In(ny)
	if local.Hour() != 8 || local.Minute() != 0 {
		t.Fatalf("next run = %s (%s in New York), want 08:00 New York", got.ScheduledFor.UTC(), local)
	}
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("next run lands on %s, want a weekday", wd)
	}
}
