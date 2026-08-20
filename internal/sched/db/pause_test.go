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
	// paused_at (#1116) stamps the pause instant — the expiry sweep counts the
	// ask window from it, so it must be "now", not the run's start.
	if got.PausedAt == nil {
		t.Fatal("paused_at must be stamped by PauseTaskForQuestion")
	} else if since := time.Since(*got.PausedAt); since < 0 || since > time.Minute {
		t.Fatalf("paused_at = %v, want ~now", *got.PausedAt)
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

// TestExpirePausedTasks (#510/#1116): a task awaiting input past the window —
// measured from paused_at, the pause instant — is failed terminally; a freshly
// paused one (however long its run executed beforehand) and a disabled window
// are left alone.
func TestExpirePausedTasks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// pause parks a task that STARTED startedAgo ago and PAUSED pausedAgo ago.
	// PauseTaskForQuestion stamps paused_at = now(), so the backdate is applied
	// directly afterwards, the same way the started_at backdate seeds the row.
	pause := func(prompt string, startedAgo, pausedAgo time.Duration) *models.Task {
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
		if pausedAgo > 0 {
			pausedAt := time.Now().Add(-pausedAgo).UTC()
			if _, err := db.Conn().ExecContext(ctx, `UPDATE tasks SET paused_at = $1 WHERE id = $2`, pausedAt, task.ID); err != nil {
				t.Fatalf("backdate paused_at: %v", err)
			}
		}
		return task
	}

	old := pause("stale question", 2*time.Hour, 2*time.Hour) // paused 120m ago
	// The #1116 regression case: a run that executed for 3h and asked its
	// question 1m ago. Under the old started_at filter this was expired on the
	// first sweep — a zero TTL for any long run's question.
	fresh := pause("fresh question on a long run", 3*time.Hour, time.Minute)

	// Disabled window is a no-op.
	if got, err := db.ExpirePausedTasks(ctx, 0); err != nil || len(got) != 0 {
		t.Fatalf("disabled window: n=%d err=%v", len(got), err)
	}

	// 60-minute window: only the stale one expires.
	expired, err := db.ExpirePausedTasks(ctx, 60)
	if err != nil {
		t.Fatalf("ExpirePausedTasks: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expired %d, want 1 (only the task PAUSED 2h ago — pause age, not run age, drives expiry)", len(expired))
	}
	if expired[0].ID != old.ID {
		t.Fatalf("expired the wrong task: got %s, want %s", expired[0].ID, old.ID)
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
	if got2, err := db.ExpirePausedTasks(ctx, 60); err != nil || len(got2) != 0 {
		t.Fatalf("second sweep: n=%d err=%v", len(got2), err)
	}
}
