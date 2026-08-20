package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// pauseTaskPastWindow adds a task, drives it running → paused_awaiting_input with
// paused_at backdated so ExpirePausedTasks' window catches it (#1116: expiry is
// measured from the pause instant, not started_at). Returns the task.
func pauseTaskPastWindow(t *testing.T, store *Storage, recurrence string) *models.Task {
	t.Helper()
	ctx := context.Background()
	database := store.DB()

	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "needs an answer",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: recurrence,
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	owner := uuid.New()
	ownerStr := owner.String()
	started := time.Now().Add(-2 * time.Hour).UTC()
	exp := time.Now().Add(5 * time.Minute).UTC()
	task.Status = models.TaskStatusRunning
	task.StartedAt = &started
	task.LeaseOwner = &ownerStr
	task.LeaseExpiresAt = &exp
	if err := database.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask(running): %v", err)
	}
	if ok, err := database.PauseTaskForQuestion(ctx, task.ID, owner, "which currency?"); err != nil || !ok {
		t.Fatalf("PauseTaskForQuestion: ok=%v err=%v", ok, err)
	}
	// PauseTaskForQuestion stamped paused_at = now(); backdate it past the
	// sweep window, the same way started_at is backdated above.
	pausedAt := time.Now().Add(-2 * time.Hour).UTC()
	if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET paused_at = $1 WHERE id = $2`, pausedAt, task.ID); err != nil {
		t.Fatalf("backdate paused_at: %v", err)
	}
	return task
}

// TestExpirePausedTasks_PreservesRecurrenceChain locks in the fix: expiring a
// paused occurrence of a RECURRING task must spawn the next occurrence, so an
// unattended overnight ask-pause no longer silently kills the whole schedule.
func TestExpirePausedTasks_PreservesRecurrenceChain(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	orig := pauseTaskPastWindow(t, store, "@daily")

	n, err := store.ExpirePausedTasks(context.Background(), 60)
	if err != nil {
		t.Fatalf("ExpirePausedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}

	// Original occurrence is terminally failed.
	gotOrig, err := store.GetTask(orig.ID)
	if err != nil {
		t.Fatalf("GetTask(orig): %v", err)
	}
	if gotOrig.Status != models.TaskStatusError {
		t.Errorf("expired occurrence status = %s, want error", gotOrig.Status)
	}

	// A successor occurrence was spawned (new row, same recurrence, not terminal).
	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	var next *models.Task
	for _, tk := range all {
		if tk.ID != orig.ID {
			next = tk
			break
		}
	}
	if next == nil {
		t.Fatal("no successor occurrence spawned — the recurrence chain died on expiry")
	}
	if next.Recurrence != "@daily" {
		t.Errorf("successor recurrence = %q, want @daily", next.Recurrence)
	}
	if next.Status.IsTerminal() {
		t.Errorf("successor status = %s, want a live (schedulable) status", next.Status)
	}
}

// TestExpirePausedTasks_NonRecurringSpawnsNothing proves a one-shot paused task
// that expires does NOT spawn a phantom successor.
func TestExpirePausedTasks_NonRecurringSpawnsNothing(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	orig := pauseTaskPastWindow(t, store, "") // no recurrence

	n, err := store.ExpirePausedTasks(context.Background(), 60)
	if err != nil {
		t.Fatalf("ExpirePausedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}

	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	if len(all) != 1 || all[0].ID != orig.ID {
		t.Fatalf("expected only the original one-shot task, got %d tasks", len(all))
	}
}
