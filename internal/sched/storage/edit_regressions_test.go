// Regression tests for three scheduler bookkeeping bugs that shared a shape:
// a write path that updated one field and left a sibling stale — a success
// after a retry keeping the retry's error_message, a gate-ended recurrence
// cancelled without completed_at, and a priority edit that never reached
// effective_priority (so it never changed dispatch order).

package storage

import (
	"context"
	"errors"
	"reflect"
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

// TaskEditFromTask is the overlay the chat manage_tasks adapter builds its
// few-field edit on so it can go through UpdateEditableTask instead of the
// unlocked UpdateTask upsert (#1104). It must carry every definition field
// through unchanged, and the write it feeds must refuse a task that has since
// been claimed — the exact rewind the upsert used to perform.
func TestTaskEditFromTaskRoundTripsAndRefusesALiveRun(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	until := future.Add(48 * time.Hour)
	remaining, maxIter, think := 3, 7, 2048
	model, fallback := "test/model", "test/fallback"
	task := &models.Task{
		ID: uuid.New(), Prompt: "definition-heavy task", Title: "Heavy", Description: "docs",
		Status: models.TaskStatusScheduled, ScheduledFor: &future, Recurrence: "0 9 * * *", Timezone: "UTC",
		RecurrenceUntil: &until, RecurrenceRemaining: &remaining,
		Model: &model, FallbackModel: &fallback, MaxIterations: &maxIter, ThinkingBudgetTokens: &think,
		Priority: models.PriorityHigh, Persona: "analyst", Tags: []string{"nightly"},
		Files: []string{"a.csv"}, FileNames: []string{"input"},
		AllowNetwork: true, CarryContext: true, InstructionSelfImprove: true,
		RunIf:         &models.RunIf{Command: "true", TimeoutSeconds: 5},
		SandboxLimits: &models.TaskSandboxLimits{MemoryMB: 512},
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("add: %v", err)
	}
	before, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	edit := TaskEditFromTask(before)
	edit.Prompt = "only the prompt changes"
	updated, err := store.UpdateEditableTask(ctx, task.ID, edit)
	if err != nil {
		t.Fatalf("UpdateEditableTask: %v", err)
	}
	if updated.Prompt != "only the prompt changes" {
		t.Fatalf("prompt not applied: %q", updated.Prompt)
	}
	after, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Everything but the prompt must survive the overlay byte-for-byte.
	before.Prompt = after.Prompt
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("TaskEditFromTask overlay changed more than the prompt:\n before=%+v\n after =%+v", before, after)
	}

	// A runner claims the task; the same edit must now be refused, not written
	// over the lease.
	owner := uuid.New()
	if _, err := store.db.Conn().ExecContext(ctx, `UPDATE tasks SET status = 'pending', scheduled_for = NULL WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if leased, err := store.leaseTaskToOwner(task.ID, owner); err != nil || leased == nil {
		t.Fatalf("lease: task=%v err=%v", leased, err)
	}
	edit.Prompt = "a stale edit against a running task"
	if _, err := store.UpdateEditableTask(ctx, task.ID, edit); !errors.Is(err, ErrTaskNotEditable) {
		t.Fatalf("edit of a running task: err=%v, want ErrTaskNotEditable", err)
	}
	live, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reload live: %v", err)
	}
	// The refused edit must not have reached the row, and the lease must be
	// intact: a write that landed here would be the #1104 shape — an edit built
	// from a stale read resurrecting status/lease columns under a live runner.
	if live.Prompt == edit.Prompt {
		t.Fatalf("refused edit still reached the live row: prompt=%q", live.Prompt)
	}
	if live.Status != models.TaskStatusLeased {
		t.Fatalf("lease was disturbed by the refused edit: status=%s, want %s", live.Status, models.TaskStatusLeased)
	}
	if live.LeaseOwner == nil || *live.LeaseOwner != owner.String() {
		t.Fatalf("lease owner lost: %v, want %s", live.LeaseOwner, owner)
	}
}
