package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestPoolClaimConcurrencyNoDoubleLease pins the headline data-integrity
// property of the claim path: under real goroutine concurrency, N workers
// racing ClaimNextPendingTask over the same pending set each lease a DISTINCT
// task — no task is ever claimed twice (FOR UPDATE SKIP LOCKED holds). The
// existing db.TestClaimNextPendingTask only drives two manually-interleaved
// transactions; this drives genuine parallelism and is meant to be run under
// -race so a torn lease assignment surfaces.
func TestPoolClaimConcurrencyNoDoubleLease(t *testing.T) {
	store := newTestStore(t)

	const (
		tasks   = 24
		workers = 8
	)
	seedPending(t, store, tasks)

	// Each worker has its own synthetic lease-owner identity (a fresh pool).
	owners := make([]string, workers)
	for i := range owners {
		owners[i] = NewPool(store, nil, Config{}).LeaseOwner().String()
	}

	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]string{} // task ID -> owner that claimed it
		dupes   int
		total   int
		wg      sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for {
				task, err := store.ClaimNextPendingTask(context.Background(), owner)
				if err != nil {
					t.Errorf("claim by %s: %v", owner, err)
					return
				}
				if task == nil {
					return // nothing left to claim
				}
				if task.Status != models.TaskStatusLeased {
					t.Errorf("claimed task %s status = %s, want leased", task.ID, task.Status)
				}
				if task.LeaseOwner == nil || *task.LeaseOwner != owner {
					t.Errorf("claimed task %s owner = %v, want %s", task.ID, task.LeaseOwner, owner)
				}
				mu.Lock()
				total++
				if prev, ok := claimed[task.ID]; ok {
					dupes++
					t.Errorf("task %s claimed twice: first by %s, again by %s", task.ID, prev, owner)
				} else {
					claimed[task.ID] = owner
				}
				mu.Unlock()
			}
		}(owners[w])
	}
	wg.Wait()

	if dupes != 0 {
		t.Fatalf("double-lease detected: %d duplicate claims", dupes)
	}
	if total != tasks {
		t.Fatalf("claimed %d times, want exactly %d (every task once, none twice)", total, tasks)
	}
	if len(claimed) != tasks {
		t.Fatalf("distinct tasks claimed = %d, want %d", len(claimed), tasks)
	}
	// And the DB agrees: every task left pending and is now leased.
	pending, _ := store.GetPendingTasks()
	if len(pending) != 0 {
		t.Fatalf("expected 0 tasks still pending after the claim storm, got %d", len(pending))
	}
}

// TestToolFailureMarksTaskFailed pins the tool-failure-mid-task contract: a
// runner whose task errors leaves the task in terminal `error` (never stuck in
// `running`), persists the error text into error_message, and clears the lease.
// A failure log session is also stored so the failure is inspectable. This is
// the runner-level analog of moc's error-status path.
func TestToolFailureMarksTaskFailed(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, errors.New("boom: tool exploded mid-task")
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool {
		failed, _ := store.GetTasksByStatus(models.TaskStatusError)
		return len(failed) == 1
	})
	cancel()
	<-done

	failed, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(failed) != 1 {
		t.Fatalf("expected 1 task in error status, got %d", len(failed))
	}
	task := failed[0]
	// Not stuck running: a terminal status with completion + cleared lease.
	if task.CompletedAt == nil {
		t.Error("failed task has no completed_at (looks stuck)")
	}
	if task.LeaseOwner != nil || task.LeaseExpiresAt != nil {
		t.Errorf("failed task lease not cleared: owner=%v expiry=%v", task.LeaseOwner, task.LeaseExpiresAt)
	}
	if task.ErrorMessage == nil || *task.ErrorMessage == "" {
		t.Fatal("failed task has no error_message persisted")
	}
	if want := "boom: tool exploded mid-task"; !strings.Contains(*task.ErrorMessage, want) {
		t.Errorf("error_message = %q, want it to contain %q", *task.ErrorMessage, want)
	}
	// No running tasks linger.
	running, _ := store.GetTasksByStatus(models.TaskStatusRunning)
	if len(running) != 0 {
		t.Errorf("expected 0 running tasks after failure, got %d", len(running))
	}
	// A failure log session was persisted (synthetic when the runner gave none).
	logs, _ := store.GetAllLogs()
	if len(logs) != 1 {
		t.Fatalf("expected 1 failure log persisted, got %d", len(logs))
	}
}

// TestInterruptedTaskPersistsTerminalOnShutdown pins the shutdown/cancel
// persistence contract for a task that does NOT finish within the grace period
// (#278): the pool force-cancels the decoupled task context when grace expires,
// the runner returns ctx.Err(), and the pool records a terminal `error`
// ("interrupted") + persists a log via the background context — the terminal
// write must survive ctx cancellation rather than being dropped. A tiny
// DrainGrace makes the force-cancel fire promptly. This complements
// TestGracefulDrain (clean-success drain within grace); here the task outlasts
// the grace and is force-cancelled.
func TestInterruptedTaskPersistsTerminalOnShutdown(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-ctx.Done() // run until the grace period expires and we're force-cancelled
		return nil, ctx.Err()
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, DrainGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started // task is in-flight
	cancel()  // begin shutdown; grace expires → task ctx cancelled → runner fails

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not drain after interrupted task finished")
	}

	// Terminal error landed via the background context despite ctx cancel.
	failed, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(failed) != 1 {
		t.Fatalf("expected 1 interrupted task in error status, got %d", len(failed))
	}
	if failed[0].ErrorMessage == nil || !strings.Contains(*failed[0].ErrorMessage, "interrupted") {
		t.Errorf("expected interrupted error message, got %v", failed[0].ErrorMessage)
	}
	// Not left running, and the failure log was persisted on the background ctx.
	running, _ := store.GetTasksByStatus(models.TaskStatusRunning)
	if len(running) != 0 {
		t.Errorf("expected 0 running tasks after interrupted drain, got %d", len(running))
	}
	logs, _ := store.GetAllLogs()
	if len(logs) != 1 {
		t.Fatalf("expected 1 log persisted on shutdown, got %d", len(logs))
	}
}

// TestStaleGoroutineTerminalWriteSkipped pins the no-double-run-clobber guard
// at the RUNNER level: if a task's lease is recovered out from under an
// in-flight goroutine (crash-recovery re-queued it and a fresh claim now owns
// it), the stale goroutine's terminal write must be SKIPPED so it cannot
// overwrite the new owner's state. This is the in-process counterpart to the
// storage-layer lease-ownership rejection (TestRecoveredTaskRejectsOldNode).
func TestStaleGoroutineTerminalWriteSkipped(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	// The runner blocks until released so we can recover the lease mid-run.
	release := make(chan struct{})
	started := make(chan struct{})
	var ranTerminal int32
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-release
		atomic.AddInt32(&ranTerminal, 1)
		return &models.LogSession{ID: "stale-run"}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started // the goroutine has claimed + is running the task

	// Find the claimed task and force-expire its lease, then recover it: this
	// simulates the renew ticker missing (a stalled process) so the backstop
	// re-queues the task even though the goroutine is still "running" it.
	var taskID uuid.UUID
	waitFor(t, time.Second, func() bool {
		running, _ := store.GetTasksByStatus(models.TaskStatusRunning)
		if len(running) == 1 {
			taskID = running[0].ID
			return true
		}
		return false
	})

	task, _ := store.GetTask(taskID)
	task.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(-time.Minute))
	if _, err := store.UpdateTask(task); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	if n, err := pool.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v, want 1, nil", n, err)
	}

	// A fresh worker re-claims the recovered task and "completes" it: this is
	// the state the stale goroutine must not clobber.
	pool2 := NewPool(store, nil, Config{MaxConcurrentAgents: 1})
	reclaimed, err := store.ClaimNextPendingTask(context.Background(), pool2.LeaseOwner().String())
	if err != nil || reclaimed == nil || reclaimed.ID != taskID {
		t.Fatalf("reclaim: task=%v err=%v", reclaimed, err)
	}
	freshResult := "done by fresh owner"
	if _, err := store.UpdateTaskStatusAtomic(taskID, pool2.LeaseOwner(), &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: &freshResult,
	}); err != nil {
		t.Fatalf("fresh-owner complete: %v", err)
	}

	// Now let the stale goroutine finish and attempt its terminal write. Two
	// guards protect the fresh owner's state: the in-process per-claim token
	// (stillOwns) for same-pool re-claims, and — as here, where a DIFFERENT
	// pool re-claimed — the storage lease-ownership check, which rejects
	// pool1's status write because the lease owner is now pool2. Either way the
	// stale goroutine must not clobber the fresh owner's terminal state.
	close(release)
	cancel()
	<-done // drain the stale goroutine through the pool's Run loop

	// The stale runner DID run to completion (so the terminal-write path was
	// reached) — the point is its write got rejected, not that it never ran.
	if atomic.LoadInt32(&ranTerminal) != 1 {
		t.Fatalf("stale runner ran %d times, want 1 (the terminal-write path must have been reached)", ranTerminal)
	}

	final, _ := store.GetTask(taskID)
	if final.Status != models.TaskStatusSuccess {
		t.Fatalf("recovered task final status = %s, want success (fresh owner's write must win)", final.Status)
	}
	if final.Result == nil || *final.Result != "done by fresh owner" {
		t.Errorf("result = %v, want the fresh owner's result (stale write clobbered it)", final.Result)
	}
	if final.LeaseOwner != nil {
		t.Errorf("completed task lease not cleared: %v", final.LeaseOwner)
	}
}
