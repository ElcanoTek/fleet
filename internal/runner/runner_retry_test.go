package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// seedTask inserts one PENDING task with explicit retry state.
func seedTask(t *testing.T, store interface {
	AddTask(*models.Task) (*models.Task, error)
}, attempt, maxRetries int) {
	t.Helper()
	task := &models.Task{
		ID: uuid.New(), Prompt: "task", Status: models.TaskStatusPending, Priority: 1,
		CreatedAt: time.Now().UTC(), AttemptCount: attempt, MaxRetries: maxRetries,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
}

func retryableErr() error {
	return fmt.Errorf("run failed: %w", agentcore.ErrRetryBudgetExhausted)
}

// TestRetryableFailureRequeuesWithBackoff: a transient (retryable) clean failure
// with retries left re-queues the SAME task to Scheduled with a future
// ScheduledFor and an incremented AttemptCount — NOT a terminal error.
func TestRetryableFailureRequeuesWithBackoff(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 2)

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, retryableErr()
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		s, _ := store.GetTasksByStatus(models.TaskStatusScheduled)
		return len(s) == 1
	})
	cancel()
	<-done

	scheduled, _ := store.GetTasksByStatus(models.TaskStatusScheduled)
	if len(scheduled) != 1 {
		t.Fatalf("expected 1 re-queued (scheduled) task, got %d", len(scheduled))
	}
	task := scheduled[0]
	if task.AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 after one transient failure", task.AttemptCount)
	}
	if task.ScheduledFor == nil || !task.ScheduledFor.After(time.Now()) {
		t.Errorf("ScheduledFor = %v, want a future backoff time", task.ScheduledFor)
	}
	if task.CompletedAt != nil {
		t.Error("a re-queued task must NOT be completed (not terminal)")
	}
	if task.LeaseOwner != nil || task.LeaseExpiresAt != nil {
		t.Error("re-queued task lease must be cleared")
	}
	// It must NOT be terminal-errored.
	if failed, _ := store.GetTasksByStatus(models.TaskStatusError); len(failed) != 0 {
		t.Errorf("a retryable failure must not be terminal; got %d errored", len(failed))
	}
}

// TestRetriesExhaustedIsDeadLettered: once AttemptCount has reached MaxRetries,
// the next transient failure is routed to the dead-letter queue (#253), not bare
// error and not another requeue. The DLQ columns record the quarantine context.
func TestRetriesExhaustedIsDeadLettered(t *testing.T) {
	store := newTestStore(t)
	// AttemptCount==MaxRetries==1 → the gate AttemptCount<MaxRetries is false.
	seedTask(t, store, 1, 1)

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, retryableErr()
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		f, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
		return len(f) == 1
	})
	cancel()
	<-done

	dlq, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
	if len(dlq) != 1 || dlq[0].CompletedAt == nil {
		t.Fatalf("exhausted retries must be dead-lettered with completed_at; got %+v", dlq)
	}
	task := dlq[0]
	if task.DeadLetteredAt == nil {
		t.Error("dead-lettered task must record dead_lettered_at")
	}
	if task.DeadLetterReason == nil || !strings.Contains(*task.DeadLetterReason, "retry budget exhausted") {
		t.Errorf("dead_letter_reason should describe retry exhaustion, got %v", task.DeadLetterReason)
	}
	// AttemptCount was 1 going in → dead_letter_attempts records the total (1+1).
	if task.DeadLetterAttempts != 2 {
		t.Errorf("dead_letter_attempts = %d, want 2 (AttemptCount+1)", task.DeadLetterAttempts)
	}
	// It must NOT be a bare error, nor re-queued.
	if f, _ := store.GetTasksByStatus(models.TaskStatusError); len(f) != 0 {
		t.Errorf("exhausted task must dead-letter, not bare error; got %d errored", len(f))
	}
	if s, _ := store.GetTasksByStatus(models.TaskStatusScheduled); len(s) != 0 {
		t.Errorf("exhausted task must not be re-queued; got %d scheduled", len(s))
	}
}

// TestNonRetryableFailureIsDeadLettered: a deterministic (non-retryable) error
// is dead-lettered immediately (#253) even when retries remain — we never re-run
// a deterministically-failing task, and a terminal failure is now reviewable.
func TestNonRetryableFailureIsDeadLettered(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 3) // 3 retries available

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, errors.New("no model configured") // deterministic, not a transient sentinel
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		f, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
		return len(f) == 1
	})
	cancel()
	<-done

	if s, _ := store.GetTasksByStatus(models.TaskStatusScheduled); len(s) != 0 {
		t.Errorf("a non-retryable error must not re-queue even with retries left; got %d scheduled", len(s))
	}
	if f, _ := store.GetTasksByStatus(models.TaskStatusError); len(f) != 0 {
		t.Errorf("a non-retryable error must dead-letter, not bare error; got %d errored", len(f))
	}
	dlq, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
	if len(dlq) != 1 {
		t.Fatalf("non-retryable error must be dead-lettered; got %d", len(dlq))
	}
	task := dlq[0]
	if task.AttemptCount != 0 {
		t.Errorf("AttemptCount should stay 0 on a non-retried terminal failure, got %d", task.AttemptCount)
	}
	// A single (first) attempt was made before quarantine.
	if task.DeadLetterAttempts != 1 {
		t.Errorf("dead_letter_attempts = %d, want 1 for an immediate non-retryable quarantine", task.DeadLetterAttempts)
	}
	if task.DeadLetterReason == nil || !strings.Contains(*task.DeadLetterReason, "non-retryable") {
		t.Errorf("dead_letter_reason should describe a non-retryable failure, got %v", task.DeadLetterReason)
	}
}

// TestReplayDeadLetteredReEnqueues: a dead-lettered task that is replayed resets
// to a fresh pending slate (attempt_count=0, DLQ columns cleared) so the
// scheduler claims it again — the replay round-trip from #253.
func TestReplayDeadLetteredReEnqueues(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 1, 1) // exhausted: AttemptCount==MaxRetries

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, retryableErr()
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		f, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
		return len(f) == 1
	})
	cancel()
	<-done

	dlq, _ := store.GetTasksByStatus(models.TaskStatusDeadLettered)
	if len(dlq) != 1 {
		t.Fatalf("setup: expected 1 dead-lettered task, got %d", len(dlq))
	}
	id := dlq[0].ID

	replayed, err := store.ReplayDeadLetteredTask(context.Background(), id)
	if err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if replayed.Status != models.TaskStatusPending {
		t.Errorf("replayed status = %s, want pending", replayed.Status)
	}
	if replayed.AttemptCount != 0 {
		t.Errorf("replayed AttemptCount = %d, want 0", replayed.AttemptCount)
	}
	if replayed.DeadLetteredAt != nil || replayed.DeadLetterReason != nil || replayed.DeadLetterAttempts != 0 {
		t.Errorf("replay must clear DLQ columns; got at=%v reason=%v attempts=%d",
			replayed.DeadLetteredAt, replayed.DeadLetterReason, replayed.DeadLetterAttempts)
	}
	if replayed.CompletedAt != nil || replayed.ErrorMessage != nil {
		t.Errorf("replay must clear completed_at/error_message; got completed=%v err=%v",
			replayed.CompletedAt, replayed.ErrorMessage)
	}

	// Persisted: a fresh read confirms the reset, and the DLQ listing is now empty.
	got, _ := store.GetTask(id)
	if got.Status != models.TaskStatusPending {
		t.Errorf("persisted status = %s, want pending", got.Status)
	}
	if remaining, _ := store.GetDeadLetteredTasks(context.Background(), 0, 0); len(remaining) != 0 {
		t.Errorf("DLQ must be empty after replay; got %d", len(remaining))
	}

	// Replaying a task that is no longer dead-lettered is rejected.
	if _, err := store.ReplayDeadLetteredTask(context.Background(), id); !errors.Is(err, storage.ErrTaskNotDeadLettered) {
		t.Errorf("replaying a non-dead-lettered task should error with ErrTaskNotDeadLettered, got %v", err)
	}
}

// TestRetriedAttemptCompletes: a previously-retried task (AttemptCount>0) that
// now succeeds lands terminal Success with its AttemptCount preserved — proving
// only the final attempt is terminal.
func TestRetriedAttemptCompletes(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 1, 2) // already retried once

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		s, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(s) == 1
	})
	cancel()
	<-done

	ok, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
	if len(ok) != 1 {
		t.Fatalf("expected 1 successful task, got %d", len(ok))
	}
	if ok[0].AttemptCount != 1 {
		t.Errorf("AttemptCount = %d, want 1 preserved through the successful retry", ok[0].AttemptCount)
	}
}

// TestRequeueRespectsLeaseOwnership: a requeue by a node that does NOT own the
// lease is rejected, leaving the task untouched (mirrors the atomic-status guard).
func TestRequeueRespectsLeaseOwnership(t *testing.T) {
	store := newTestStore(t)
	owner := uuid.New()
	// Claim the task so it has a real lease owner, then attempt a stale requeue.
	seedTask(t, store, 0, 2)
	claimed, err := store.ClaimNextPendingTask(context.Background(), owner.String())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (task=%v)", err, claimed)
	}

	stale := uuid.New()
	if _, err := store.RequeueTaskForRetryWithContext(context.Background(), claimed.ID, stale, time.Now().Add(time.Minute), "x"); err == nil {
		t.Fatal("requeue by a non-owning node must be rejected")
	}
	// The owner CAN requeue it.
	if _, err := store.RequeueTaskForRetryWithContext(context.Background(), claimed.ID, owner, time.Now().Add(time.Minute), "retry"); err != nil {
		t.Fatalf("owner requeue should succeed: %v", err)
	}
	got, _ := store.GetTask(claimed.ID)
	if got.Status != models.TaskStatusScheduled || got.AttemptCount != 1 {
		t.Errorf("after owner requeue: status=%s attempt=%d, want scheduled/1", got.Status, got.AttemptCount)
	}
}
