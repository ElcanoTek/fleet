package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestReclaimCancelsStaleZombieRun covers the MAJORITY lease-loss ordering on
// a single-box deployment (#1116): the lease expires (DB outage / stalled
// renewals), recovery re-queues the task, and the SAME pool re-claims it
// within a poll tick — overwriting the active-map entry — before the zombie's
// next renewal fires. The zombie must still be cancelled (its external side
// effects stop), and the fresh run must be untouched and complete normally. A
// live-map lookup guard cannot provide this: after the overwrite the map holds
// only the fresh claim, so the zombie's cancel must be reachable without it —
// at overwrite time in tryClaim, and via the snapshotted per-claim cancel in
// renewActiveLeases.
func TestReclaimCancelsStaleZombieRun(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)
	task := tasks[0]
	// Recovery must RE-QUEUE (not dead-letter) for the re-claim to happen.
	task.MaxRetries = 3
	if _, err := store.UpdateTask(task); err != nil {
		t.Fatalf("set max_retries: %v", err)
	}

	var mu sync.Mutex
	var ctxs []context.Context
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		mu.Lock()
		ctxs = append(ctxs, ctx)
		mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &models.LogSession{ID: "s-fresh"}, nil
		}
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	poolCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	nCtxs := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(ctxs)
	}
	waitFor(t, 2*time.Second, func() bool { return nCtxs() == 1 })

	// The renewals "stall": force-expire the lease and run the recovery
	// backstop, re-queueing the task while run #1 is still executing.
	row, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	row.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(-time.Minute))
	if _, err := store.UpdateTask(row); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	if n, err := pool.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v, want 1, nil", n, err)
	}

	// The same pool re-claims within a poll tick: run #2 starts and the
	// active-map entry now belongs to the fresh claim.
	waitFor(t, 2*time.Second, func() bool { return nCtxs() == 2 })

	// The zombie's renewal fires only now — after the overwrite.
	pool.renewActiveLeases()

	mu.Lock()
	zombieCtx, freshCtx := ctxs[0], ctxs[1]
	mu.Unlock()

	select {
	case <-zombieCtx.Done():
		// Cancelled — external side effects stop.
	case <-time.After(2 * time.Second):
		t.Fatal("zombie run kept executing after its task was re-claimed by the same pool — the exact pre-fix double-execution")
	}
	select {
	case <-freshCtx.Done():
		t.Fatal("fresh run was cancelled — the stale claim's cancellation must never touch the re-claim")
	default:
	}

	// The fresh run completes normally and lands the terminal state.
	close(release)
	waitFor(t, 2*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got.Status == models.TaskStatusSuccess
	})

	cancel()
	<-done
}
