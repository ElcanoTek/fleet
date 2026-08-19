package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRenewalCancelsRunAfterLeaseLoss locks in the #1116 zombie-run fix: when
// a lease renewal comes back ErrTaskLeaseNotHeld — the row no longer carries
// this run's token, so recovery has re-queued the task (a fresh attempt may
// already be executing) — the local run's context must be cancelled so its
// EXTERNAL side effects stop, instead of executing to natural completion in
// parallel with the fresh attempt. The DB writes were always token-fenced;
// the cancel is what bounds the side effects.
//
// A second, still-owned task renews in the same pass and must NOT be touched:
// the cancel fires only on the definite not-held verdict.
func TestRenewalCancelsRunAfterLeaseLoss(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 2)

	// Fenced fake: each run parks until its context dies or the test releases
	// it, recording its context so the test can observe the cancellation.
	var mu sync.Mutex
	runCtxs := make(map[uuid.UUID]context.Context)
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, task *models.Task) (*models.LogSession, error) {
		mu.Lock()
		runCtxs[task.ID] = ctx
		mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &models.LogSession{ID: "s-" + task.ID.String()}, nil
		}
	})

	// LeaseRenewInterval is an hour: the test drives renewActiveLeases itself
	// so the not-held verdict lands deterministically.
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(runCtxs) == 2
	})

	// Steal the first task's lease: recovery + a fresh claim persist a NEW
	// owner token, exactly what this run's next renewal collides with.
	stolen := tasks[0]
	if _, err := store.DB().Conn().ExecContext(context.Background(),
		`UPDATE tasks SET lease_owner = $1 WHERE id = $2`, uuid.New().String(), stolen.ID); err != nil {
		t.Fatalf("steal lease: %v", err)
	}

	pool.renewActiveLeases()

	mu.Lock()
	stolenCtx := runCtxs[stolen.ID]
	keptCtx := runCtxs[tasks[1].ID]
	mu.Unlock()

	select {
	case <-stolenCtx.Done():
		// The zombie run was cancelled — external side effects stop here.
	case <-time.After(2 * time.Second):
		t.Fatal("run kept executing after its lease renewal returned not-held — zombie side effects would continue in parallel with the fresh attempt")
	}
	select {
	case <-keptCtx.Done():
		t.Fatal("still-owned run was cancelled — only the definite not-held verdict may cancel")
	default:
	}

	// The persisted transcript records the HONEST reason (#1116 review): the
	// run was cancelled because its lease was lost — not misattributed to a
	// server shutdown (the pre-fix interrupted branch's only story).
	waitFor(t, 2*time.Second, func() bool {
		session, err := store.GetLog(stolen.ID)
		if err != nil || session == nil {
			return false
		}
		for _, m := range session.Messages {
			if strings.Contains(m.Content, "lease was lost") {
				return true
			}
			if strings.Contains(m.Content, "server shutdown") {
				t.Fatalf("zombie transcript misattributes the cancel to a shutdown: %q", m.Content)
			}
		}
		return false
	})

	// The still-owned run finishes normally after release.
	close(release)
	waitFor(t, 2*time.Second, func() bool {
		got, err := store.GetTask(tasks[1].ID)
		return err == nil && got.Status == models.TaskStatusSuccess
	})

	cancel()
	<-done

	// The fenced writes held: the stolen task's row still belongs to the
	// "fresh" owner and was never flipped terminal by the cancelled zombie.
	got, err := store.GetTask(stolen.ID)
	if err != nil {
		t.Fatalf("GetTask(stolen): %v", err)
	}
	if got.Status.IsTerminal() {
		t.Fatalf("stolen task status = %s; the cancelled zombie must not have written a terminal state", got.Status)
	}
}
