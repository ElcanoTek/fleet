package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// #510: ask → pause (lease released) → resume with answer → run consumes + clears.
func TestTaskPauseResumeLifecycle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	owner := uuid.New()
	ownerStr := owner.String()
	task := &models.Task{ID: uuid.New(), Prompt: "reconcile invoices", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	// Simulate a claimed, running task under `owner`'s lease.
	exp := time.Now().Add(5 * time.Minute).UTC()
	task.Status = models.TaskStatusRunning
	task.LeaseOwner = &ownerStr
	task.LeaseExpiresAt = &exp
	if err := db.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask(running): %v", err)
	}

	// Pause requires the holder's lease + running status.
	if ok, err := db.PauseTaskForQuestion(ctx, task.ID, uuid.New(), "which currency?"); err != nil || ok {
		t.Fatalf("pause with wrong lease must not apply: ok=%v err=%v", ok, err)
	}
	ok, err := db.PauseTaskForQuestion(ctx, task.ID, owner, "which currency?")
	if err != nil || !ok {
		t.Fatalf("pause: ok=%v err=%v", ok, err)
	}
	got, _ := db.GetTask(ctx, task.ID)
	if got.Status != models.TaskStatusPausedAwaitingInput {
		t.Fatalf("status = %s; want paused", got.Status)
	}
	if got.PendingQuestion != "which currency?" {
		t.Fatalf("question not stored: %q", got.PendingQuestion)
	}
	if got.LeaseOwner != nil || got.LeaseExpiresAt != nil {
		t.Fatalf("paused task must hold NO lease (no sandbox): owner=%v exp=%v", got.LeaseOwner, got.LeaseExpiresAt)
	}
	if got.Status.IsTerminal() {
		t.Fatal("paused must NOT be terminal (it resumes)")
	}

	// Paused appears in the awaiting-input queue.
	paused, _ := db.ListPausedTasks(ctx, 10)
	if len(paused) != 1 || paused[0].ID != task.ID {
		t.Fatalf("paused queue: %+v", paused)
	}

	// Resume with an answer re-queues it (pending) and carries the answer.
	if ok, err := db.ResumeTask(ctx, task.ID, "USD"); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}
	got, _ = db.GetTask(ctx, task.ID)
	if got.Status != models.TaskStatusPending || got.PendingAnswer != "USD" || got.PendingQuestion != "which currency?" {
		t.Fatalf("after resume: %+v", got)
	}
	// A second resume on a non-paused task is a no-op.
	if ok, _ := db.ResumeTask(ctx, task.ID, "EUR"); ok {
		t.Fatal("resume on a non-paused task must not apply")
	}

	// The resumed run claims (lease) and clears the Q&A.
	task2, _ := db.GetTask(ctx, task.ID)
	task2.Status = models.TaskStatusRunning
	task2.LeaseOwner = &ownerStr
	_ = db.UpdateTask(ctx, task2)
	if err := db.ClearPendingQA(ctx, task.ID, owner); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = db.GetTask(ctx, task.ID)
	if got.PendingQuestion != "" || got.PendingAnswer != "" {
		t.Fatalf("Q&A must clear after the run consumes it: %+v", got)
	}
}

// TestExpirePausedTasks (#510): a task awaiting input past the window is failed
// terminally; a fresh one and a disabled window are left alone.
func TestExpirePausedTasks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	pause := func(prompt string, startedAgo time.Duration) *models.Task {
		t.Helper()
		owner := uuid.New()
		ownerStr := owner.String()
		task := &models.Task{ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		started := time.Now().Add(-startedAgo).UTC()
		exp := time.Now().Add(5 * time.Minute).UTC()
		task.Status = models.TaskStatusRunning
		task.StartedAt = &started
		task.LeaseOwner = &ownerStr
		task.LeaseExpiresAt = &exp
		if err := db.UpdateTask(ctx, task); err != nil {
			t.Fatalf("UpdateTask(running): %v", err)
		}
		if ok, err := db.PauseTaskForQuestion(ctx, task.ID, owner, "which currency?"); err != nil || !ok {
			t.Fatalf("pause: ok=%v err=%v", ok, err)
		}
		return task
	}

	old := pause("stale question", 2*time.Hour)     // started 120m ago
	fresh := pause("fresh question", 1*time.Minute) // started 1m ago

	// Disabled window is a no-op.
	if n, err := db.ExpirePausedTasks(ctx, 0); err != nil || n != 0 {
		t.Fatalf("disabled window: n=%d err=%v", n, err)
	}

	// 60-minute window: only the stale one expires.
	n, err := db.ExpirePausedTasks(ctx, 60)
	if err != nil {
		t.Fatalf("ExpirePausedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1 (only the 2h-old paused task)", n)
	}

	gotOld, _ := db.GetTask(ctx, old.ID)
	if gotOld.Status != models.TaskStatusError || !gotOld.Status.IsTerminal() {
		t.Errorf("stale paused task status = %s, want terminal error", gotOld.Status)
	}
	if gotOld.CompletedAt == nil {
		t.Error("expired task should have completed_at stamped")
	}
	if gotOld.ErrorMessage == nil || *gotOld.ErrorMessage == "" || gotOld.PendingQuestion != "" {
		t.Errorf("expired task should carry an error and clear the question: q=%q", gotOld.PendingQuestion)
	}

	gotFresh, _ := db.GetTask(ctx, fresh.ID)
	if gotFresh.Status != models.TaskStatusPausedAwaitingInput {
		t.Errorf("fresh paused task status = %s, want still paused", gotFresh.Status)
	}

	// Idempotent: a second sweep finds nothing new.
	if n2, err := db.ExpirePausedTasks(ctx, 60); err != nil || n2 != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n2, err)
	}
}
