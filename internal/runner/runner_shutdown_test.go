package runner

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestForceCancelDrainsImmediately pins the fast-exit path (#278): ForceCancel
// cancels the in-flight task context at once — independent of the (long) grace
// period — so a SIGINT / listener-error shutdown drains promptly instead of
// blocking out the whole grace window.
func TestForceCancelDrainsImmediately(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-ctx.Done() // exits only when the task context is cancelled
		return nil, ctx.Err()
	})

	// A deliberately long grace: were ForceCancel a no-op, the drain would block
	// ~10s and the test's 2s deadline would fire.
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		DrainGrace:          10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started
	cancel()           // stop the claim loop / begin drain
	pool.ForceCancel() // immediately cancel the in-flight task context

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ForceCancel did not drain the pool promptly (waited on the grace period?)")
	}
}

// TestActiveTasks pins the diagnostic counter behind the SIGUSR1 status log: it
// reflects exactly the tasks currently executing.
func TestActiveTasks(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-release
		return &models.LogSession{ID: "s"}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started
	if got := pool.ActiveTasks(); got != 1 {
		t.Fatalf("ActiveTasks while running = %d, want 1", got)
	}

	close(release)
	cancel()
	<-done
	if got := pool.ActiveTasks(); got != 0 {
		t.Errorf("ActiveTasks after drain = %d, want 0", got)
	}
}

// TestDrainWaitsForNaturalCompletionWithinGrace pins the core graceful-shutdown
// contract (#278): a task that finishes within the grace period keeps its real
// outcome (success) rather than being marked interrupted — the decoupled task
// context is NOT cancelled by the claim-ctx shutdown.
func TestDrainWaitsForNaturalCompletionWithinGrace(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		// Finish shortly AFTER shutdown begins but well within the grace period.
		// If the claim-ctx cancel leaked into the task context, ctx.Err() would be
		// non-nil here and the task would be (wrongly) recorded as interrupted.
		time.Sleep(60 * time.Millisecond)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &models.LogSession{ID: "s"}, nil
	})

	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		DrainGrace:          5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started
	cancel() // begin shutdown while the task is still running

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pool did not drain")
	}

	waitFor(t, time.Second, func() bool {
		success, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(success) == 1
	})
	failed, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(failed) != 0 {
		t.Errorf("task finishing within grace was recorded as error (%d); want success", len(failed))
	}
}
