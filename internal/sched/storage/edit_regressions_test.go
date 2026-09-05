// Regression tests for three scheduler bookkeeping bugs that shared a shape:
// a write path that updated one field and left a sibling stale — a success
// after a retry keeping the retry's error_message, a gate-ended recurrence
// cancelled without completed_at, and a priority edit that never reached
// effective_priority (so it never changed dispatch order).

package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestSuccessAfterRetryClearsErrorMessage(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := uuid.New()

	task := &models.Task{ID: uuid.New(), Prompt: "flaky then fine", Status: models.TaskStatusPending, Priority: models.PriorityNormal, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := store.leaseTaskToOwner(task.ID, owner); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Attempt 1 fails cleanly and is requeued with the failure reason.
	if _, err := store.RequeueTaskForRetryWithContext(ctx, task.ID, owner, time.Now().UTC().Add(-time.Second), "attempt 1: upstream 503"); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	// The scheduler promotes the due retry back to pending; attempt 2 is
	// claimed and succeeds.
	if _, err := store.db.Conn().ExecContext(ctx, `UPDATE tasks SET status = 'pending' WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("promote retry: %v", err)
	}
	if leased, err := store.leaseTaskToOwner(task.ID, owner); err != nil || leased == nil {
		t.Fatalf("lease 2: task=%v err=%v", leased, err)
	}
	msg := "done"
	got, err := store.UpdateTaskStatusAtomicWithContext(ctx, task.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: &msg})
	if err != nil {
		t.Fatalf("success: %v", err)
	}
	if got.Status != models.TaskStatusSuccess {
		t.Fatalf("status = %s, want success", got.Status)
	}
	if got.ErrorMessage != nil {
		t.Fatalf("success row still carries the retry's error_message %q", *got.ErrorMessage)
	}
	reloaded, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ErrorMessage != nil {
		t.Fatalf("persisted success row still carries error_message %q", *reloaded.ErrorMessage)
	}
}

func TestSettleGatedTaskTerminalStampsCompletedAt(t *testing.T) {
	store, _ := newTestStore(t)
	when := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	mk := func(prompt string) *models.Task {
		ts := when
		task := &models.Task{ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusScheduled, Priority: models.PriorityNormal, CreatedAt: time.Now().UTC(), ScheduledFor: &ts,
			RunIf: &models.RunIf{Command: "true", TimeoutSeconds: 5}}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add %s: %v", prompt, err)
		}
		return task
	}
	ended := mk("recurrence ended at its gate")
	promoted := mk("gate passed")

	if n, err := store.SettleGatedTask(ended.ID, ended.ScheduledFor, models.TaskStatusCancelled); err != nil || n != 1 {
		t.Fatalf("settle cancelled: n=%d err=%v", n, err)
	}
	got, err := store.GetTask(ended.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != models.TaskStatusCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatal("a recurrence cancelled at its gate has no completed_at — the retention sweeps would never prune it")
	}

	// The non-terminal settle (gate passed → pending) must NOT stamp it.
	if n, err := store.SettleGatedTask(promoted.ID, promoted.ScheduledFor, models.TaskStatusPending); err != nil || n != 1 {
		t.Fatalf("settle pending: n=%d err=%v", n, err)
	}
	got, err = store.GetTask(promoted.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != models.TaskStatusPending || got.CompletedAt != nil {
		t.Fatalf("pending settle: status=%s completed_at=%v, want pending / nil", got.Status, got.CompletedAt)
	}
}

func TestPriorityEditChangesDispatchOrder(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	mk := func(prompt string, prio int) *models.Task {
		task := &models.Task{ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusPending, Priority: prio, EffectivePriority: prio, CreatedAt: time.Now().UTC()}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add %s: %v", prompt, err)
		}
		return task
	}
	first := mk("bulk that will be starvation-promoted", models.PriorityBulk)
	second := mk("was bulk", models.PriorityBulk)

	// Promote the bulk task to critical via the edit path.
	edit := TaskEdit{Prompt: second.Prompt, Priority: models.PriorityCritical}
	edited, err := store.UpdateEditableTask(ctx, second.ID, edit)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Priority != models.PriorityCritical || edited.EffectivePriority != models.PriorityCritical {
		t.Fatalf("edited priority=%d effective=%d, want both %d", edited.Priority, edited.EffectivePriority, models.PriorityCritical)
	}
	reloaded, err := store.GetTask(second.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.EffectivePriority != models.PriorityCritical {
		t.Fatalf("persisted effective_priority = %d, want %d (the edit never reached dispatch order)", reloaded.EffectivePriority, models.PriorityCritical)
	}

	claimed, err := store.ClaimNextPendingTask(ctx, "worker")
	if err != nil || claimed == nil {
		t.Fatalf("claim: task=%v err=%v", claimed, err)
	}
	if claimed.ID != second.ID {
		t.Fatalf("claimed %q first; the task edited to critical (%q) should have won", claimed.Prompt, second.Prompt)
	}

	// A starvation promotion (to the floor, PriorityHigh) more urgent than the
	// NEW priority survives an edit to a less urgent one: the sweep would
	// re-promote it anyway.
	if _, err := store.db.Conn().ExecContext(ctx, `UPDATE tasks SET effective_priority = $1 WHERE id = $2`, models.StarvationFloorPriority, first.ID); err != nil {
		t.Fatalf("simulate starvation promotion: %v", err)
	}
	edited, err = store.UpdateEditableTask(ctx, first.ID, TaskEdit{Prompt: first.Prompt, Priority: models.PriorityLow})
	if err != nil {
		t.Fatalf("re-prioritize promoted task: %v", err)
	}
	want := min(models.StarvationFloorPriority, models.PriorityLow)
	if edited.EffectivePriority != want {
		t.Fatalf("effective_priority after edit = %d, want %d (keep the more urgent of the promotion and the new priority)", edited.EffectivePriority, want)
	}
}
