package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// loseFirstResult makes the first landed attempt of the named outcome write
// report failure instead, as when the connection drops after the database
// applied the write but before its answer arrived. It counts every attempt
// of that write.
func loseFirstResult(pool *Pool, what string, lost error) *atomic.Int32 {
	var attempts atomic.Int32
	var dropped atomic.Bool
	pool.afterOutcomeAttempt = func(w string, err error) error {
		if w != what {
			return err
		}
		attempts.Add(1)
		if err == nil && dropped.CompareAndSwap(false, true) {
			return lost
		}
		return err
	}
	return &attempts
}

// A COMMIT whose answer is lost is ErrTaskCommitOutcomeUnknown. The read-back
// finds this run's token on the success row, so the outcome counts as
// recorded: no second write, one notification, and the agent's summary stays
// the row's result (no fallback overwrote it).
func TestOutcomeLostCommitResultIsRecognisedOnReadBack(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String(), Messages: []models.LogMessage{
			{ID: "a1", Role: "assistant", Content: "Report sent to the team."},
		}}, nil
	}), Config{Notifier: notif})
	reset := &pgconn.PgError{Severity: "FATAL", Code: "08006", Message: "connection reset at commit"}
	attempts := loseFirstResult(pool, "success", fmt.Errorf("%w: %w", storage.ErrTaskCommitOutcomeUnknown, reset))
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusSuccess)
	if got.Result == nil || *got.Result != "Report sent to the team." {
		t.Errorf("result = %v, want the agent's summary", got.Result)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("success write attempts = %d, want 1 (the read-back settled it)", n)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusSuccess {
		t.Errorf("notification = %+v, want one success", ev)
	}
	if status := retainedTerminalStatus(t, pool, task.ID); status != "succeeded" {
		t.Errorf("terminal stream status = %q, want succeeded", status)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// A park is a single autocommit UPDATE: a lost answer surfaces as a plain
// connection error. The retry then finds the lease already released (the
// park landed), and the read-back recognises it — the run is parked, not
// handed to lease recovery.
func TestOutcomeLostParkResultIsRecognisedOnReadBack(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	var calls atomic.Int32
	var pool *Pool
	pool = newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		if err := pool.wakeHandlerFor(tk)(tools.WakeSpec{WakeAt: time.Now().Add(time.Hour), Note: "later"}); err != nil {
			return nil, err
		}
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{})
	attempts := loseFirstResult(pool, "wake park", &pgconn.PgError{Severity: "FATAL", Code: "08006", Message: "connection reset"})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusPausedAwaitingWake)
	if got.WakeCycles != 1 {
		t.Errorf("wake_cycles = %d, want 1 (one park, not two)", got.WakeCycles)
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("park write attempts = %d, want 2 (the lost one, then the fenced retry the read-back settled)", n)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// An operator cancel that lands while the outcome write is failing takes the
// row: the retry's fenced refusal reads back as someone else's outcome, so
// the run stops retrying and fires nothing.
func TestOutcomeRetryStopsWhenTheLeaseIsTaken(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	failStatusWrites(t, store, task.ID, 1<<30, "57P01", models.TaskStatusSuccess)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif})
	var retrying atomic.Int32
	pool.afterOutcomeAttempt = func(_ string, err error) error {
		if err != nil {
			retrying.Add(1)
		}
		return err
	}
	stop := runPool(t, pool)

	waitFor(t, 5*time.Second, func() bool { return retrying.Load() >= 2 })
	if _, err := store.CancelTaskAtomic(task.ID, "stopped by test"); err != nil {
		t.Fatalf("CancelTaskAtomic: %v", err)
	}
	waitRunReturned(t, pool, &calls)
	assertOutcome(t, store, task.ID, models.TaskStatusCancelled)
	stop()
	if got := notif.drain(); len(got) != 0 {
		t.Fatalf("a cancelled run fired %d notification(s): %+v", len(got), got)
	}
}

// When the database stays unreachable through shutdown, the outcome cannot
// be written. The run was stamped finished first, so lease recovery
// dead-letters it for review — it is not run again — while a run that never
// ended (a crash) is still re-queued.
func TestOutcomeUnwrittenAtShutdownIsDeadLetteredNotRerun(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	attempts := failStatusWrites(t, store, task.ID, 1<<30, "57P01", models.TaskStatusSuccess)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif, DrainGrace: 50 * time.Millisecond})
	pool.outcomeShutdownWindow = 200 * time.Millisecond
	stop := runPool(t, pool)

	waitFor(t, 5*time.Second, func() bool { return attempts() >= 3 })
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not end the outcome retries within the shutdown window")
	}

	row, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusRunning || row.LeaseOwner == nil ||
		row.FinishedRunID == nil || *row.FinishedRunID != *row.LeaseOwner {
		t.Fatalf("after shutdown: status=%s lease_owner=%v finished_run_id=%v, want running and stamped finished by its own claim",
			row.Status, row.LeaseOwner, row.FinishedRunID)
	}

	dropStatusWriteFailures(t, store) // the database is back; the server restarts
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpiredLeases(); err != nil || n != 0 {
		t.Fatalf("RecoverExpiredLeases = %d, %v; want nothing re-queued", n, err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskStatusDeadLettered || got.AttemptCount != 0 ||
		got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "outcome not recorded") {
		t.Fatalf("recovered: status=%s attempt_count=%d reason=%v, want dead-lettered as an unrecorded outcome",
			got.Status, got.AttemptCount, got.DeadLetterReason)
	}
	if got.LeaseOwner != nil || got.FinishedRunID != nil {
		t.Errorf("recovered row keeps lease_owner=%v finished_run_id=%v, want both cleared", got.LeaseOwner, got.FinishedRunID)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
	if got := notif.drain(); len(got) != 0 {
		t.Fatalf("an unrecorded outcome fired %d notification(s): %+v", len(got), got)
	}
}

// outageSpanningLeaseExpiry drives the bug's own trigger to its worst case:
// the run ends, the database goes away before its success write lands, and
// stays away past the lease (renewals fail with it, so the lease runs out).
// It starts the pool itself, after the write failure is in place: a pool
// already running can claim the task, end the run and land its success write
// before the failure is installed, and then nothing is left to retry.
// It returns once the run is retrying with an expired lease: the claim token
// the row is leased by, and the pool's stop.
func outageSpanningLeaseExpiry(t *testing.T, store *storage.Storage, pool *Pool, taskID uuid.UUID) (token string, stop func()) {
	t.Helper()
	attempts := failStatusWrites(t, store, taskID, 1<<30, "57P01", models.TaskStatusSuccess)
	stop = runPool(t, pool)
	waitFor(t, 5*time.Second, func() bool { return attempts() >= 3 })
	row, err := store.GetTask(taskID)
	if err != nil || row.Status != models.TaskStatusRunning || row.LeaseOwner == nil {
		t.Fatalf("while the success write fails: %+v, %v; want running under its lease", row, err)
	}
	if !slices.Contains(pool.HeldLeaseTokens(), *row.LeaseOwner) {
		t.Fatalf("held lease tokens %v do not include the retrying run's %s", pool.HeldLeaseTokens(), *row.LeaseOwner)
	}
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, taskID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	return *row.LeaseOwner, stop
}

// When the database comes back after an outage longer than the lease, this
// process's own lease recovery (the scheduler tick) may run before the run's
// next attempt. It must leave the row alone — the run is alive and holds it —
// and the run's write then lands: the outcome is recorded once, and the task
// is not run again.
func TestOutageSpanningLeaseExpiryIsLeftToTheRunningRun(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String(), Messages: []models.LogMessage{
			{ID: "a1", Role: "assistant", Content: "Report sent to the team."},
		}}, nil
	}), Config{Notifier: notif})
	store.SetLeaseHolder(pool.HeldLeaseTokens) // as cmd/fleet wires the scheduler's storage

	token, stop := outageSpanningLeaseExpiry(t, store, pool, task.ID)

	// The database answers again, and the sweep gets there first.
	if n, err := store.RecoverExpiredLeases(); err != nil || n != 0 {
		t.Fatalf("RecoverExpiredLeases = %d, %v; want nothing re-queued", n, err)
	}
	row, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusRunning || row.LeaseOwner == nil || *row.LeaseOwner != token || row.AttemptCount != 0 {
		t.Fatalf("after the sweep: status=%s lease_owner=%v attempt_count=%d, want the run's row untouched",
			row.Status, row.LeaseOwner, row.AttemptCount)
	}

	// Then the run's own write reaches it.
	dropStatusWriteFailures(t, store)
	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusSuccess)
	if got.Result == nil || *got.Result != "Report sent to the team." {
		t.Errorf("result = %v, want the agent's summary", got.Result)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusSuccess {
		t.Errorf("notification = %+v, want one success", ev)
	}
	// The idle claim loop holds a fresh token for the instant of each claim
	// attempt (held before the claim commits), so the set need not be empty:
	// what matters is that this run's token is no longer held.
	if held := pool.HeldLeaseTokens(); slices.Contains(held, token) {
		t.Errorf("held lease tokens after the run returned = %v, still include the run's %s", held, token)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// Another server's lease recovery does not know which runs this process
// holds. If it reaches the row first, the finished stamp still keeps the run
// from running again: it is dead-lettered as an unrecorded outcome, and the
// run's own retry, finding the row taken, records and fires nothing.
func TestOutageSpanningLeaseExpiryRecoveredElsewhereIsNotRerun(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif})
	store.SetLeaseHolder(pool.HeldLeaseTokens)

	_, stop := outageSpanningLeaseExpiry(t, store, pool, task.ID)

	elsewhere := storage.New() // a second server on the same database
	elsewhere.SetDatabase(store.DB())
	if n, err := elsewhere.RecoverExpiredLeases(); err != nil || n != 0 {
		t.Fatalf("remote RecoverExpiredLeases = %d, %v; want nothing re-queued", n, err)
	}
	dropStatusWriteFailures(t, store)
	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusDeadLettered)
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "outcome not recorded") {
		t.Errorf("dead_letter_reason = %v, want the unrecorded-outcome reason", got.DeadLetterReason)
	}
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
	if evs := notif.drain(); len(evs) != 0 {
		t.Fatalf("a run whose row was taken fired %d notification(s): %+v", len(evs), evs)
	}
}

// A run that asked to run again (a retry requeue) is not stamped finished
// first: if its requeue is still failing when shutdown ends the retries,
// lease recovery re-queues it like a crash — the retry it asked for — rather
// than dead-lettering it as an unrecorded outcome.
func TestUnwrittenRetryRequeueIsRequeuedByRecovery(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 2)
	attempts := failStatusWrites(t, store, task.ID, 1<<30, "57P01", models.TaskStatusScheduled)

	var calls atomic.Int32
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return nil, retryableErr()
	}), Config{DrainGrace: 50 * time.Millisecond})
	pool.outcomeShutdownWindow = 200 * time.Millisecond
	stop := runPool(t, pool)

	waitFor(t, 5*time.Second, func() bool { return attempts() >= 3 })
	stop()

	row, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusRunning || row.FinishedRunID != nil {
		t.Fatalf("after shutdown: status=%s finished_run_id=%v, want running and not marked finished", row.Status, row.FinishedRunID)
	}
	dropStatusWriteFailures(t, store)
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, %v; want the requeue the run asked for", n, err)
	}
	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskStatusPending || got.AttemptCount != 1 {
		t.Fatalf("recovered: status=%s attempt_count=%d, want pending as attempt 1", got.Status, got.AttemptCount)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}
