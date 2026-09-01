package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestKickWakesClaimLoopAheadOfPoll covers the synchronous-create dispatch
// path (#1279): a task made claimable between poll ticks is picked up by an
// explicit Kick instead of waiting out pollInterval. The poll interval is set
// far beyond the test budget so any claim after the startup scan can ONLY
// come from the kick — the negative window in the middle proves the poll
// isn't doing the work.
func TestKickWakesClaimLoopAheadOfPoll(t *testing.T) {
	store := newTestStore(t)

	var ran int32
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		atomic.AddInt32(&ran, 1)
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: time.Hour, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	// Let the startup scan pass over the empty queue first, so the task seeded
	// below cannot be claimed by anything but the kick (or the hour-away tick).
	time.Sleep(500 * time.Millisecond)
	seedPending(t, store, 1)
	time.Sleep(400 * time.Millisecond)
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Fatalf("task ran %d time(s) without a kick — the startup scan raced the seed and this test proves nothing", n)
	}

	// A burst of kicks must neither block nor panic (1-buffered, coalescing),
	// and must produce exactly one run for the one pending task.
	for i := 0; i < 5; i++ {
		pool.Kick()
	}
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&ran) == 1 })

	cancel()
	<-done
	if n := atomic.LoadInt32(&ran); n != 1 {
		t.Fatalf("coalesced kicks must claim the task exactly once, ran %d", n)
	}
}
