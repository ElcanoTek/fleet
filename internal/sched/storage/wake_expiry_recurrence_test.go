package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// parkStrandedWake adds a task, drives it running → paused_awaiting_wake, then
// strands it: paused_at well past the grace and wake_at NULL, the shape
// WakeDueTasks (which filters `wake_at IS NOT NULL`) can never reach.
func parkStrandedWake(t *testing.T, store *Storage, recurrence string) *models.Task {
	t.Helper()
	ctx := context.Background()
	database := store.DB()

	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "waiting for an event that never comes",
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
	exp := time.Now().Add(5 * time.Minute).UTC()
	task.Status = models.TaskStatusRunning
	task.LeaseOwner = &ownerStr
	task.LeaseExpiresAt = &exp
	if err := database.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask(running): %v", err)
	}
	if ok, err := database.PauseTaskForWake(ctx, task.ID, owner, time.Now().Add(time.Hour).UTC(), "invoice.paid", "waiting"); err != nil || !ok {
		t.Fatalf("PauseTaskForWake: ok=%v err=%v", ok, err)
	}
	if _, err := database.Conn().ExecContext(ctx,
		`UPDATE tasks SET paused_at = $1, wake_at = NULL WHERE id = $2`,
		time.Now().Add(-48*time.Hour).UTC(), task.ID); err != nil {
		t.Fatalf("strand the row: %v", err)
	}
	return task
}

// TestExpireStrandedWakeTasks_PreservesRecurrenceChain: expiring a stranded
// occurrence of a RECURRING task must spawn the next occurrence, exactly as the
// paused-awaiting-input expiry does. Without it, one unreachable park would
// silently end a daily schedule — the same failure #1116 fixed for the other
// parked state.
func TestExpireStrandedWakeTasks_PreservesRecurrenceChain(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	orig := parkStrandedWake(t, store, "@daily")

	n, err := store.ExpireStrandedWakeTasks(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatalf("ExpireStrandedWakeTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}

	gotOrig, err := store.GetTask(orig.ID)
	if err != nil {
		t.Fatalf("GetTask(orig): %v", err)
	}
	if gotOrig.Status != models.TaskStatusError {
		t.Errorf("expired occurrence status = %s, want error", gotOrig.Status)
	}

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
		t.Fatal("no successor occurrence spawned — the recurrence chain died on a stranded wake")
	}
	if next.Recurrence != "@daily" {
		t.Errorf("successor recurrence = %q, want @daily", next.Recurrence)
	}
	if next.Status.IsTerminal() {
		t.Errorf("successor status = %s, want a live (schedulable) status", next.Status)
	}
}

func TestExpireStrandedWakeTasks_NonRecurringSpawnsNothing(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")

	orig := parkStrandedWake(t, store, "")

	if n, err := store.ExpireStrandedWakeTasks(context.Background(), 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("ExpireStrandedWakeTasks: n=%d err=%v", n, err)
	}
	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	if len(all) != 1 || all[0].ID != orig.ID {
		t.Fatalf("expected only the original one-shot task, got %d tasks", len(all))
	}
}
