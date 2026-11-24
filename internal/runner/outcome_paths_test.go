package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// A structured task whose success write is refused takes the persistence
// class; with retry_on covering it, the run asks to run again. Its success
// attempt had already stamped the run finished, so the requeue clears that
// stamp first: if the requeue never lands before shutdown, lease recovery
// re-queues the task as the run asked, rather than dead-lettering it.
func TestRefusedStructuredSuccessRetriedIsRequeuedWhenTheRequeueNeverLands(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := &models.Task{
		ID: uuid.New(), Prompt: "return a structured answer", Status: models.TaskStatusPending,
		Priority: 1, CreatedAt: time.Now().UTC(), OutputSchema: json.RawMessage(runnerOutputSchema),
		MaxRetries: 1, RetryPolicy: &models.RetryPolicy{RetryOn: []string{models.FailureOutputPersist}},
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	attempts := failStatusWrites(t, store, task.ID, 1<<30, "57P01", models.TaskStatusScheduled)

	var calls atomic.Int32
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{Messages: []models.LogMessage{{Role: "assistant", Content: `{"answer":42}`}}}, nil
	}), Config{DrainGrace: 50 * time.Millisecond})
	pool.outcomeShutdownWindow = 200 * time.Millisecond
	const constraint = "runner_test_refuse_structured_success"
	pool.beforeSuccessCommit = func(*models.Task, uuid.UUID) {
		if _, err := store.DB().Conn().ExecContext(context.Background(),
			"ALTER TABLE tasks ADD CONSTRAINT "+constraint+" CHECK (status <> 'success') NOT VALID"); err != nil {
			t.Errorf("add constraint: %v", err)
		}
	}
	stop := runPool(t, pool)

	waitFor(t, 5*time.Second, func() bool { return attempts() >= 3 })
	stop()

	row, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != models.TaskStatusRunning || row.FinishedRunID != nil {
		t.Fatalf("after shutdown: status=%s finished_run_id=%v, want running with the success attempt's stamp cleared", row.Status, row.FinishedRunID)
	}
	if _, err := store.DB().Conn().ExecContext(t.Context(), "ALTER TABLE tasks DROP CONSTRAINT IF EXISTS "+constraint); err != nil {
		t.Fatal(err)
	}
	dropStatusWriteFailures(t, store)
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, %v; want the retry the run asked for", n, err)
	}
	if got, _ := store.GetTask(task.ID); got.Status != models.TaskStatusPending || got.AttemptCount != 1 {
		t.Fatalf("recovered: status=%s attempt_count=%d, want pending as attempt 1", got.Status, got.AttemptCount)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// A read-back that fails transiently is retried like the write: here the
// first success write landed with its answer lost, the read-back after it
// failed too, and the next attempt's fenced refusal reads back as landed.
func TestOutcomeReadBackTransientErrorIsRetried(t *testing.T) {
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
	attempts := loseFirstResult(pool, "success", fmt.Errorf("%w: %w", storage.ErrTaskCommitOutcomeUnknown,
		&pgconn.PgError{Severity: "FATAL", Code: "08006", Message: "connection reset at commit"}))
	var readBacks atomic.Int32
	pool.afterReadBack = func(err error) error {
		if readBacks.Add(1) == 1 {
			return &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}
		}
		return err
	}
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusSuccess)
	if got.Result == nil || *got.Result != "Report sent to the team." {
		t.Errorf("result = %v, want the agent's summary", got.Result)
	}
	if a, r := attempts.Load(), readBacks.Load(); a != 2 || r != 2 {
		t.Errorf("success write attempts = %d, read-backs = %d; want 2 and 2", a, r)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusSuccess {
		t.Errorf("notification = %+v, want one success", ev)
	}
	stop()
}

// When the fence refuses an outcome write and the row cannot be read back
// for a reason a retry cannot fix, nothing proves the outcome is this run's:
// the run stops at once — no retry loop until shutdown, no fallback write, no
// notification.
func TestOutcomeReadBackPermanentErrorAfterFenceRefusalIsLeaseLost(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif})
	const constraint = "runner_test_refuse_success_readback"
	pool.beforeSuccessCommit = func(*models.Task, uuid.UUID) {
		if _, err := store.DB().Conn().ExecContext(context.Background(),
			"ALTER TABLE tasks ADD CONSTRAINT "+constraint+" CHECK (status <> 'success') NOT VALID"); err != nil {
			t.Errorf("add constraint: %v", err)
		}
	}
	var success, fallback atomic.Int32
	pool.afterOutcomeAttempt = func(what string, err error) error {
		switch what {
		case "success":
			success.Add(1)
			if err != nil {
				return storage.ErrTaskLeaseNotHeld // the fence refused it
			}
		case "unrecordable success":
			fallback.Add(1)
		}
		return err
	}
	pool.afterReadBack = func(error) error { return errors.New("permission denied for table tasks") }
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	if _, err := store.DB().Conn().ExecContext(t.Context(), "ALTER TABLE tasks DROP CONSTRAINT IF EXISTS "+constraint); err != nil {
		t.Fatal(err)
	}
	assertOutcome(t, store, task.ID, models.TaskStatusRunning) // nothing written over it
	if s, f := success.Load(), fallback.Load(); s != 1 || f != 0 {
		t.Errorf("success attempts = %d, fallback attempts = %d; want 1 and 0", s, f)
	}
	stop()
	time.Sleep(200 * time.Millisecond) // a notification would be fired off-thread
	if evs := notif.drain(); len(evs) != 0 {
		t.Fatalf("an outcome that is not this run's fired %d notification(s): %+v", len(evs), evs)
	}
}

// The ask pause tells a person the task needs their answer only once the
// park landed; a park that did not land asks nobody.
func TestAskPauseThatDidNotLandNotifiesNobody(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	failStatusWrites(t, store, task.ID, 1<<30, "23514", models.TaskStatusPausedAwaitingInput) // refused for good

	var calls atomic.Int32
	notif := &fakeNotifier{}
	var pool *Pool
	pool = newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		pool.mu.Lock()
		pool.pauseRequested[tk.ID] = "which environment?"
		pool.mu.Unlock()
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	assertOutcome(t, store, task.ID, models.TaskStatusRunning)
	stop()
	time.Sleep(200 * time.Millisecond) // a notification would be fired off-thread
	if evs := notif.drain(); len(evs) != 0 {
		t.Fatalf("a park that did not land sent %d notification(s): %+v", len(evs), evs)
	}
}

// claimRunningRow takes one new task through the production claim and
// running report under a fresh token, as a worker does.
func claimRunningRow(t *testing.T, store *storage.Storage) (*models.Task, uuid.UUID) {
	t.Helper()
	task := seedOutcomeTask(t, store, 3)
	token := uuid.New()
	claimed, err := store.ClaimNextPendingTask(context.Background(), token.String())
	if err != nil || claimed == nil || claimed.ID != task.ID {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, token, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("running: %v", err)
	}
	return task, token
}

// The log line an outcome write leaves when shutdown ends its retries says
// what lease recovery will do with the row: dead-letter it (the run is
// marked finished), re-queue it (a run that asked to run again), or recover
// it like a crash (the run could not be marked finished).
//
// Not parallel: it swaps the process default logger to capture the lines.
func TestOutcomeShutdownMessagesSayWhatRecoveryWillDo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	store := newTestStore(t)
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(context.Context, *models.Task) (*models.LogSession, error) {
		return nil, nil
	}), Config{})
	pool.outcomeShutdownWindow = time.Millisecond
	pool.signalForceStop() // shutdown already ended the retries
	failing := func(context.Context) (*models.Task, error) {
		return nil, &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection"}
	}

	cases := []struct {
		name  string
		rerun bool
		owner bool // whether the write's token holds the row's lease
		want  string
	}{
		{"marked finished", false, true, "the run is marked finished, so lease recovery will dead-letter it"},
		{"asked to run again", true, true, "re-run request still not recorded at shutdown"},
		{"could not be marked", false, false, "the run could not be marked finished"},
	}
	for _, tc := range cases {
		buf.Reset()
		task, token := claimRunningRow(t, store)
		if !tc.owner {
			token = uuid.New()
		}
		_, result, _ := pool.recordOutcome(context.Background(), task.ID, token, outcomeWrite{
			what: "success", status: models.TaskStatusSuccess, rerun: tc.rerun, apply: failing,
		})
		if result != outcomeUnwritten {
			t.Errorf("%s: result = %s, want unwritten", tc.name, result)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s: shutdown log = %q, want it to contain %q", tc.name, buf.String(), tc.want)
		}
	}
}
