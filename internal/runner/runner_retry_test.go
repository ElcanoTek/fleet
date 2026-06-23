package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
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

// TestRetriesExhaustedIsTerminal: once AttemptCount has reached MaxRetries, the
// next failure is terminal (error), not another requeue.
func TestRetriesExhaustedIsTerminal(t *testing.T) {
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
		f, _ := store.GetTasksByStatus(models.TaskStatusError)
		return len(f) == 1
	})
	cancel()
	<-done

	failed, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(failed) != 1 || failed[0].CompletedAt == nil {
		t.Fatalf("exhausted retries must be terminal error with completed_at; got %+v", failed)
	}
	if s, _ := store.GetTasksByStatus(models.TaskStatusScheduled); len(s) != 0 {
		t.Errorf("exhausted task must not be re-queued; got %d scheduled", len(s))
	}
}

// TestNonRetryableFailureIsTerminalEvenWithRetriesLeft: a deterministic
// (non-retryable) error fails terminally even when retries remain — we never
// re-run a deterministically-failing task.
func TestNonRetryableFailureIsTerminalEvenWithRetriesLeft(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 3) // 3 retries available

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, errors.New("no model configured") // deterministic, not a transient sentinel
	}), Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 3*time.Second, func() bool {
		f, _ := store.GetTasksByStatus(models.TaskStatusError)
		return len(f) == 1
	})
	cancel()
	<-done

	if s, _ := store.GetTasksByStatus(models.TaskStatusScheduled); len(s) != 0 {
		t.Errorf("a non-retryable error must not re-queue even with retries left; got %d scheduled", len(s))
	}
	failed, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(failed) != 1 {
		t.Fatalf("non-retryable error must be terminal; got %d errored", len(failed))
	}
	if failed[0].AttemptCount != 0 {
		t.Errorf("AttemptCount should stay 0 on a non-retried terminal failure, got %d", failed[0].AttemptCount)
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
