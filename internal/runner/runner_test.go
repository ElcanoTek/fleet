package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// newTestStore initializes a sched storage against DATABASE_URL, serialized on
// the shared advisory lock so it never races the storage/db package tests.
func newTestStore(t *testing.T) *storage.Storage {
	t.Helper()
	database := db.New()
	if err := database.Init(""); err != nil {
		t.Skipf("Skipping runner test because DB init failed: %v", err)
	}

	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("Failed to get DB connection for lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("Failed to acquire test lock: %v", err)
	}

	cleanup := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM nodes", "DELETE FROM users"} {
			database.Conn().ExecContext(ctx, q)
		}
	}
	cleanup()
	t.Cleanup(func() {
		cleanup()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		conn.Close()
		database.Close()
	})

	store := storage.New()
	store.SetDatabase(database)
	return store
}

func seedPending(t *testing.T, store *storage.Storage, n int) []*models.Task {
	t.Helper()
	tasks := make([]*models.Task, n)
	for i := 0; i < n; i++ {
		task := &models.Task{ID: uuid.New(), Prompt: "task", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		tasks[i] = task
	}
	return tasks
}

// TestPoolClaimsAndCompletes verifies the pool claims a pending task, runs it
// through the TaskRunner, and writes a terminal success status + log.
func TestPoolClaimsAndCompletes(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	var ran int32
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		atomic.AddInt32(&ran, 1)
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&ran) == 1 })
	// Give the terminal write a moment.
	waitFor(t, time.Second, func() bool {
		tasks, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(tasks) == 1
	})
	cancel()
	<-done

	success, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
	if len(success) != 1 {
		t.Fatalf("Expected 1 success task, got %d", len(success))
	}
	logs, _ := store.GetAllLogs()
	if len(logs) != 1 {
		t.Fatalf("Expected 1 log, got %d", len(logs))
	}
}

// TestPoolCapSaturation asserts the global semaphore bounds concurrency: with
// cap=N and N+1 pending tasks, at most N tasks run concurrently and the extra
// stays pending until a slot frees.
func TestPoolCapSaturation(t *testing.T) {
	store := newTestStore(t)
	const capacity = 2
	seedPending(t, store, capacity+1)

	var concurrent int32
	var maxConcurrent int32
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		c := atomic.AddInt32(&concurrent, 1)
		for {
			m := atomic.LoadInt32(&maxConcurrent)
			if c <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, c) {
				break
			}
		}
		<-release // block so all in-flight tasks overlap
		atomic.AddInt32(&concurrent, -1)
		return &models.LogSession{ID: "s"}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: capacity, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	if pool.Cap() != capacity {
		t.Fatalf("Cap() = %d, want %d", pool.Cap(), capacity)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	// Wait until the cap is saturated (exactly cap tasks blocked in Run).
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&concurrent) == capacity })

	// Give the pool extra ticks to (incorrectly) over-claim if the cap leaked.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&concurrent); got != capacity {
		t.Fatalf("concurrency exceeded cap: got %d concurrent, cap %d", got, capacity)
	}
	// Exactly one task must still be pending (N+1 seeded, N running).
	pending, _ := store.GetPendingTasks()
	if len(pending) != 1 {
		t.Fatalf("Expected 1 task still pending under cap, got %d", len(pending))
	}

	close(release) // let them all finish
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&concurrent) == 0 })
	if got := atomic.LoadInt32(&maxConcurrent); got != capacity {
		t.Errorf("max observed concurrency = %d, want exactly %d", got, capacity)
	}
	cancel()
	<-done
}

// TestRestartMidTaskRecovery simulates a systemd restart mid-task: a task is
// claimed+leased, the "worker" dies (we drop the lease to the past), recovery
// re-queues it, and a fresh claim succeeds.
func TestRestartMidTaskRecovery(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)
	taskID := tasks[0].ID

	pool := NewPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		return nil, nil
	}), Config{MaxConcurrentAgents: 1})

	// Worker claims + leases the task, then "crashes" before completing.
	claimed, err := store.ClaimNextPendingTask(context.Background(), pool.LeaseOwner().String())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != taskID {
		t.Fatalf("expected to claim the seeded task")
	}
	if claimed.Status != models.TaskStatusLeased {
		t.Fatalf("expected leased, got %s", claimed.Status)
	}

	// Advance the clock past the lease window by force-expiring the lease (the
	// process is gone, so no renewal happens).
	claimed.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(-time.Minute))
	if _, err := store.UpdateTask(claimed); err != nil {
		t.Fatalf("force-expire: %v", err)
	}

	// Recovery re-queues the task.
	n, err := pool.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 recovered task, got %d", n)
	}
	recovered, _ := store.GetTask(taskID)
	if recovered.Status != models.TaskStatusPending {
		t.Fatalf("expected pending after recovery, got %s", recovered.Status)
	}
	if recovered.LeaseOwner != nil {
		t.Fatalf("expected lease cleared after recovery")
	}

	// A fresh process (new lease owner) can re-claim it.
	pool2 := NewPool(store, nil, Config{MaxConcurrentAgents: 1})
	reclaimed, err := store.ClaimNextPendingTask(context.Background(), pool2.LeaseOwner().String())
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if reclaimed == nil || reclaimed.ID != taskID {
		t.Fatalf("expected to re-claim the recovered task")
	}
}

// TestPerTaskMCPSelectionReachesRunner verifies the claimed task carries its
// mcp_selection through to the TaskRunner unchanged — this is what drives the
// scheduled driver to bind the right server + account per task.
func TestPerTaskMCPSelectionReachesRunner(t *testing.T) {
	store := newTestStore(t)

	sel := models.MCPSelection{{Server: "xandr", Account: "client_a"}, {Server: "magnite"}}
	task := &models.Task{ID: uuid.New(), Prompt: "sel", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(), MCPSelection: sel}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	got := make(chan models.MCPSelection, 1)
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		got <- task.MCPSelection
		return &models.LogSession{ID: "s"}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	select {
	case received := <-got:
		if len(received) != 2 || received[0].Server != "xandr" || received[0].Account != "client_a" || received[1].Server != "magnite" {
			t.Fatalf("runner saw wrong selection: %+v", received)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the task to reach the runner")
	}
	cancel()
	<-done
}

// TestGracefulDrain asserts Shutdown (ctx cancel) waits for the in-flight task
// to finish and its terminal status/log to land via the background context.
func TestGracefulDrain(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	finish := make(chan struct{})
	var wrote int32
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-finish // still running when shutdown begins
		atomic.StoreInt32(&wrote, 1)
		return &models.LogSession{ID: "s"}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started // task is in-flight
	cancel()  // begin shutdown while the task runs

	// Run must NOT return until the in-flight task finishes (taskWG drains).
	select {
	case <-done:
		t.Fatal("pool drained before in-flight task finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(finish) // let the task complete
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not drain after task finished")
	}

	if atomic.LoadInt32(&wrote) != 1 {
		t.Fatal("in-flight task did not complete during drain")
	}
	// Terminal status + log landed via the background context.
	waitFor(t, time.Second, func() bool {
		success, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(success) == 1
	})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", d)
	}
}

func ptrTime(tm time.Time) *time.Time { return &tm }
