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
