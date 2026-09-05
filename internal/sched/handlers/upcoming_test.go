// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestProjectRuns_RecurringCapsAtFive(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	task := &models.Task{ID: uuid.New(), Name: "hourly", Prompt: "p", Recurrence: "0 * * * *"}
	runs := projectRuns(task, now, time.Time{})
	if len(runs) != upcomingPerTaskMax {
		t.Fatalf("expected %d occurrences, got %d", upcomingPerTaskMax, len(runs))
	}
	// Sorted ascending, all in the future, all flagged recurring.
	prev := now
	for _, r := range runs {
		if !r.Recurring {
			t.Fatalf("expected recurring=true, got false")
		}
		if !r.NextRun.After(prev) {
			t.Fatalf("occurrences not strictly increasing: %v then %v", prev, r.NextRun)
		}
		prev = r.NextRun
	}
}

func TestProjectRuns_RecurringHonorsTimezone(t *testing.T) {
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// 09:00 daily in New York → not 09:00 UTC.
	task := &models.Task{ID: uuid.New(), Recurrence: "0 9 * * *", Timezone: "America/New_York"}
	runs := projectRuns(task, now, time.Time{})
	if len(runs) == 0 {
		t.Fatal("expected occurrences")
	}
	loc, _ := time.LoadLocation("America/New_York")
	got := runs[0].NextRun.In(loc)
	if got.Hour() != 9 {
		t.Fatalf("expected 09:00 in New York, got %02d:00 (%v)", got.Hour(), got)
	}
}

func TestProjectRuns_OneShotFutureAndPast(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	past := now.Add(-2 * time.Hour)

	fut := projectRuns(&models.Task{ID: uuid.New(), ScheduledFor: &future}, now, time.Time{})
	if len(fut) != 1 || fut[0].Recurring {
		t.Fatalf("expected 1 non-recurring future run, got %+v", fut)
	}
	if !fut[0].NextRun.Equal(future) {
		t.Fatalf("expected next_run=%v, got %v", future, fut[0].NextRun)
	}

	if pastRuns := projectRuns(&models.Task{ID: uuid.New(), ScheduledFor: &past}, now, time.Time{}); len(pastRuns) != 0 {
		t.Fatalf("expected past one-shot to project no runs, got %d", len(pastRuns))
	}
}

func TestProjectRuns_InvalidCronYieldsNothing(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	if runs := projectRuns(&models.Task{ID: uuid.New(), Recurrence: "not a cron"}, now, time.Time{}); len(runs) != 0 {
		t.Fatalf("expected 0 runs for an invalid cron, got %d", len(runs))
	}
}

func TestTaskLocation_FallsBackToUTC(t *testing.T) {
	if loc := taskLocation(""); loc != time.UTC {
		t.Fatalf("empty tz should be UTC, got %v", loc)
	}
	if loc := taskLocation("Nowhere/Bogus"); loc != time.UTC {
		t.Fatalf("bogus tz should fall back to UTC, got %v", loc)
	}
	if loc := taskLocation("America/New_York"); loc == time.UTC {
		t.Fatal("valid tz should not fall back to UTC")
	}
}

// TestGetUpcomingRuns_HandlerReturnsSortedFeed seeds a recurring and a future
// one-shot task and asserts the endpoint returns them sorted by next_run.
func TestGetUpcomingRuns_HandlerReturnsSortedFeed(t *testing.T) {
	_, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()

	mux := chi.NewRouter()
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, nil)
	mux.Group(func(rt chi.Router) {
		rt.Use(h.AdminAuthMiddleware)
		rt.Get("/tasks/upcoming", h.GetUpcomingRuns)
	})

	future := time.Now().UTC().Add(30 * time.Minute)
	recurring := &models.Task{ID: uuid.New(), Name: "daily", Prompt: "p", Status: models.TaskStatusScheduled, Recurrence: "0 9 * * *", Priority: 1, CreatedAt: time.Now().UTC()}
	oneShot := &models.Task{ID: uuid.New(), Name: "once", Prompt: "p", Status: models.TaskStatusScheduled, ScheduledFor: &future, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(recurring); err != nil {
		t.Fatalf("AddTask recurring: %v", err)
	}
	if _, err := store.AddTask(oneShot); err != nil {
		t.Fatalf("AddTask oneShot: %v", err)
	}

	req := httptest.NewRequest("GET", "/tasks/upcoming?limit=10", nil)
	req.Header.Set("X-API-Key", "test-admin-key")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Upcoming []UpcomingRun `json:"upcoming"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Upcoming) < 2 {
		t.Fatalf("expected at least 2 upcoming runs, got %d", len(resp.Upcoming))
	}
	for i := 1; i < len(resp.Upcoming); i++ {
		if resp.Upcoming[i].NextRun.Before(resp.Upcoming[i-1].NextRun) {
			t.Fatalf("runs not sorted ascending at %d", i)
		}
	}
}

func TestProjectRuns_HorizonProjectsWholeWindow(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	task := &models.Task{ID: uuid.New(), Recurrence: "0 * * * *"} // hourly
	horizon := now.Add(24 * time.Hour)
	runs := projectRuns(task, now, horizon)
	if len(runs) != 24 {
		t.Fatalf("expected 24 hourly occurrences inside the horizon, got %d", len(runs))
	}
	for _, r := range runs {
		if r.NextRun.After(horizon) {
			t.Fatalf("occurrence %v is past the horizon %v", r.NextRun, horizon)
		}
	}
}

func TestProjectRuns_HonorsRecurrenceUntil(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	until := now.Add(3*time.Hour + 30*time.Minute)
	task := &models.Task{ID: uuid.New(), Recurrence: "0 * * * *", RecurrenceUntil: &until}
	// Even with a week-long horizon, nothing past the task's own end date.
	runs := projectRuns(task, now, now.Add(7*24*time.Hour))
	if len(runs) != 3 {
		t.Fatalf("expected 3 occurrences up to recurrence_until, got %d", len(runs))
	}
	// Count-based mode honors it too.
	if legacy := projectRuns(task, now, time.Time{}); len(legacy) != 3 {
		t.Fatalf("expected 3 occurrences in count mode, got %d", len(legacy))
	}
}

func TestProjectRuns_HonorsRecurrenceRemaining(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	remaining := 2
	task := &models.Task{ID: uuid.New(), Recurrence: "0 * * * *", RecurrenceRemaining: &remaining}
	if runs := projectRuns(task, now, now.Add(7*24*time.Hour)); len(runs) != 2 {
		t.Fatalf("expected the 2-run budget to cap the horizon projection, got %d", len(runs))
	}
	if runs := projectRuns(task, now, time.Time{}); len(runs) != 2 {
		t.Fatalf("expected the 2-run budget to cap the count projection, got %d", len(runs))
	}
}

// The row's scheduled_for is what the scheduler promotes on, so a recurring
// task that was postponed (an edit, a run_if skip backoff, a reconcile) must
// project THAT instant first and walk the cron from it. Seeding from now
// showed a run the scheduler would never make.
func TestProjectRuns_RecurringSeedsFromScheduledFor(t *testing.T) {
	now := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	// Daily at 09:00, but the row was pushed out to July 2nd 14:00 — past the
	// July 1st and July 2nd 09:00 fires a walk from now would have invented.
	postponed := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	task := &models.Task{ID: uuid.New(), Recurrence: "0 9 * * *", Timezone: "UTC", ScheduledFor: &postponed}
	runs := projectRuns(task, now, time.Time{})
	if len(runs) != upcomingPerTaskMax {
		t.Fatalf("expected %d occurrences, got %d", upcomingPerTaskMax, len(runs))
	}
	if !runs[0].NextRun.Equal(postponed) || !runs[0].Recurring {
		t.Fatalf("first occurrence = %v (recurring=%v), want the row's scheduled_for %v", runs[0].NextRun, runs[0].Recurring, postponed)
	}
	if want := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC); !runs[1].NextRun.Equal(want) {
		t.Fatalf("second occurrence = %v, want the first cron fire after scheduled_for (%v)", runs[1].NextRun, want)
	}

	// A due row (scheduled_for in the past) is about to be promoted; the walk
	// falls back to now, as the one-shot branch does.
	due := now.Add(-time.Hour)
	task.ScheduledFor = &due
	runs = projectRuns(task, now, time.Time{})
	if len(runs) == 0 || !runs[0].NextRun.Equal(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("due row should walk from now, got %+v", runs)
	}

	// The horizon and the recurrence_until end condition apply to the seeded
	// occurrence too.
	task.ScheduledFor = &postponed
	if runs := projectRuns(task, now, now.Add(24*time.Hour)); len(runs) != 0 {
		t.Fatalf("scheduled_for beyond the horizon must project nothing, got %+v", runs)
	}
}
