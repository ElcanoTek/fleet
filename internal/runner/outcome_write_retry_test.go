package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The outcome write of a finished run (success, a terminal failure, a
// dead-letter, a retry requeue, a park) used to be a single attempt: a
// database restart or connection reset at that instant left the row
// `running`, and lease recovery ran the whole task again five minutes later.
// These tests fail the write the way such an outage does and assert the
// outcome lands once the database answers — and that lease recovery then has
// nothing to run again.

// failStatusWrites makes the next n writes that move taskID into one of
// statuses fail with SQLSTATE code: 57P01 is what a Postgres restart reports,
// 08006 a lost connection, 40001 a lost serialization race. A BEFORE UPDATE
// trigger raises it, and a sequence counts the matching writes, because
// nextval survives the failed transaction's rollback. Only a status CHANGE
// matches, so lease renewals and the finished stamp pass. Each test runs on a
// database clone of its own, so the trigger reaches no other test. The
// returned func reports how many matching writes were attempted so far.
func failStatusWrites(t *testing.T, store *storage.Storage, taskID uuid.UUID, n int, code string, statuses ...models.TaskStatus) func() int {
	t.Helper()
	quoted := make([]string, len(statuses))
	for i, s := range statuses {
		quoted[i] = "'" + string(s) + "'"
	}
	conn := store.DB().Conn()
	for _, stmt := range []string{
		`CREATE SEQUENCE runner_test_status_writes`,
		fmt.Sprintf(`CREATE FUNCTION runner_test_fail_status_write() RETURNS trigger AS $$
			BEGIN
				IF NEW.id = '%s'::uuid AND NEW.status IN (%s) AND OLD.status IS DISTINCT FROM NEW.status THEN
					IF nextval('runner_test_status_writes') <= %d THEN
						RAISE EXCEPTION 'injected write failure' USING ERRCODE = '%s';
					END IF;
				END IF;
				RETURN NEW;
			END
			$$ LANGUAGE plpgsql`, taskID, strings.Join(quoted, ", "), n, code),
		`CREATE TRIGGER runner_test_fail_status_write BEFORE UPDATE ON tasks
			FOR EACH ROW EXECUTE FUNCTION runner_test_fail_status_write()`,
	} {
		if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("install write failure: %v", err)
		}
	}
	return func() int {
		var last int64
		var called bool
		if err := conn.QueryRowContext(context.Background(),
			`SELECT last_value, is_called FROM runner_test_status_writes`).Scan(&last, &called); err != nil {
			t.Errorf("read the write count: %v", err)
			return -1
		}
		if !called {
			return 0
		}
		return int(last)
	}
}

// dropStatusWriteFailures removes failStatusWrites' trigger: the database
// is healthy again.
func dropStatusWriteFailures(t *testing.T, store *storage.Storage) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS runner_test_fail_status_write ON tasks`,
		`DROP FUNCTION IF EXISTS runner_test_fail_status_write()`,
		`DROP SEQUENCE IF EXISTS runner_test_status_writes`,
	} {
		if _, err := store.DB().Conn().ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("remove write failure: %v", err)
		}
	}
}

// seedOutcomeTask adds one pending task with the given retry budget.
func seedOutcomeTask(t *testing.T, store *storage.Storage, maxRetries int) *models.Task {
	t.Helper()
	task := &models.Task{
		ID: uuid.New(), Prompt: "send the weekly report", Status: models.TaskStatusPending,
		Priority: 1, CreatedAt: time.Now().UTC(), MaxRetries: maxRetries,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// runPool runs pool until the returned stop is called (or the test ends).
func runPool(t *testing.T, pool *Pool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// waitRunReturned waits until the runner has been called and the worker has
// returned (its active entry is gone): whatever outcome the run recorded is
// now on the row.
func waitRunReturned(t *testing.T, pool *Pool, calls *atomic.Int32) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool { return calls.Load() >= 1 && pool.ActiveTasks() == 0 })
}

// assertOutcome checks the row holds the outcome the run reached.
func assertOutcome(t *testing.T, store *storage.Storage, id uuid.UUID, want models.TaskStatus) *models.Task {
	t.Helper()
	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != want {
		t.Errorf("after the run returned: status = %s, want %s (its outcome write failed and was not retried)", got.Status, want)
	}
	return got
}

// assertNotRunAgain is the bug's own probe: it expires whatever lease the row
// still holds and runs lease recovery, as the scheduler tick does once a
// worker stops renewing. A run whose outcome was recorded comes through
// untouched — not re-queued for a second run of its emails and MCP writes.
func assertNotRunAgain(t *testing.T, store *storage.Storage, id uuid.UUID) {
	t.Helper()
	before, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if _, err := store.DB().Conn().ExecContext(t.Context(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 second' WHERE id = $1 AND lease_owner IS NOT NULL`, id); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	n, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases: %v", err)
	}
	after, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if n != 0 || after.Status != before.Status || after.AttemptCount != before.AttemptCount {
		t.Fatalf("lease recovery re-queued a finished run: requeued=%d, status %s→%s, attempt_count %d→%d",
			n, before.Status, after.Status, before.AttemptCount, after.AttemptCount)
	}
}

// waitOneNotification waits for the run's notification and checks it is the
// only one.
func waitOneNotification(t *testing.T, notif *fakeNotifier) notify.Event {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool { return len(notif.drain()) >= 1 })
	got := notif.drain()
	if len(got) != 1 {
		t.Fatalf("notifications = %d (%+v), want 1", len(got), got)
	}
	return got[0]
}

func TestOutcomeWriteRetried_Success(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	attempts := failStatusWrites(t, store, task.ID, 2, "57P01", models.TaskStatusSuccess)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String(), Messages: []models.LogMessage{
			{ID: "a1", Role: "assistant", Content: "Report sent to the team."},
		}}, nil
	}), Config{Notifier: notif})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusSuccess)
	if got.Result == nil || *got.Result != "Report sent to the team." {
		t.Errorf("result = %v, want the agent's summary", got.Result)
	}
	if n := attempts(); n != 3 {
		t.Errorf("success write attempts = %d, want 3 (two failures, then it landed)", n)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusSuccess {
		t.Errorf("notification = %+v, want one success", ev)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

func TestOutcomeWriteRetried_DeadLetter(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	// The dead-letter AND its error fallback fail: a database that is down
	// fails every write, not just the first.
	attempts := failStatusWrites(t, store, task.ID, 2, "08006", models.TaskStatusDeadLettered, models.TaskStatusError)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return nil, errors.New("no model configured") // deterministic: dead-letters on the first attempt
	}), Config{Notifier: notif})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusDeadLettered)
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "non-retryable") {
		t.Errorf("dead_letter_reason = %v, want the run's own non-retryable reason", got.DeadLetterReason)
	}
	if n := attempts(); n != 3 {
		t.Errorf("outcome write attempts = %d, want 3", n)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusFailure {
		t.Errorf("notification = %+v, want one failure", ev)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

func TestOutcomeWriteRetried_RetryRequeue(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 2)
	attempts := failStatusWrites(t, store, task.ID, 2, "40001", models.TaskStatusScheduled, models.TaskStatusError)

	var calls atomic.Int32
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return nil, retryableErr()
	}), Config{})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusScheduled)
	if got.AttemptCount != 1 || got.ScheduledFor == nil || !got.ScheduledFor.After(time.Now()) {
		t.Errorf("requeue: attempt_count=%d scheduled_for=%v, want attempt 1 with a future backoff", got.AttemptCount, got.ScheduledFor)
	}
	if n := attempts(); n != 3 {
		t.Errorf("outcome write attempts = %d, want 3", n)
	}
	// The requeue is the run's own retry, after its backoff; lease recovery
	// must not add another, earlier one.
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

func TestOutcomeWriteRetried_AuditAbort(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	attempts := failStatusWrites(t, store, task.ID, 2, "57P01", models.TaskStatusError)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()},
			fmt.Errorf("%w: page unchanged, nothing published", agentcore.ErrAuditAborted)
	}), Config{Notifier: notif})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusError)
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "self-audit") {
		t.Errorf("error_message = %v, want the self-audit abort", got.ErrorMessage)
	}
	if n := attempts(); n != 3 {
		t.Errorf("outcome write attempts = %d, want 3", n)
	}
	waitOneNotification(t, notif)
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1 (an audit abort is never retried)", n)
	}
}

func TestOutcomeWriteRetried_WallTimeout(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	attempts := failStatusWrites(t, store, task.ID, 2, "08006", models.TaskStatusError)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}), Config{Notifier: notif, WallClockTimeout: 100 * time.Millisecond})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusError)
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "wall-clock timeout") {
		t.Errorf("error_message = %v, want the wall-clock timeout", got.ErrorMessage)
	}
	if n := attempts(); n != 3 {
		t.Errorf("outcome write attempts = %d, want 3", n)
	}
	waitOneNotification(t, notif)
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1 (a wall-clock timeout is never retried)", n)
	}
}

func TestOutcomeWriteRetried_Interrupted(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)
	attempts := failStatusWrites(t, store, task.ID, 2, "57P01", models.TaskStatusError)

	var calls atomic.Int32
	started := make(chan struct{})
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}), Config{DrainGrace: -1}) // shutdown force-cancels at once
	stop := runPool(t, pool)

	<-started
	stop() // the run is interrupted; its outcome is written in the shutdown window
	got := assertOutcome(t, store, task.ID, models.TaskStatusError)
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "interrupted") {
		t.Errorf("error_message = %v, want the interruption", got.ErrorMessage)
	}
	if n := attempts(); n != 3 {
		t.Errorf("outcome write attempts = %d, want 3", n)
	}
	assertNotRunAgain(t, store, task.ID)
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

func TestOutcomeWriteRetried_WakePark(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	attempts := failStatusWrites(t, store, task.ID, 2, "57P01", models.TaskStatusPausedAwaitingWake)

	var calls atomic.Int32
	var pool *Pool
	pool = newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		if err := pool.wakeHandlerFor(tk)(tools.WakeSpec{WakeAt: time.Now().Add(time.Hour), Note: "check the deploy"}); err != nil {
			return nil, err
		}
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{})
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	got := assertOutcome(t, store, task.ID, models.TaskStatusPausedAwaitingWake)
	if got.WakeCycles != 1 || got.WakeNote != "check the deploy" {
		t.Errorf("park: wake_cycles=%d wake_note=%q, want one park carrying the note", got.WakeCycles, got.WakeNote)
	}
	if n := attempts(); n != 3 {
		t.Errorf("park write attempts = %d, want 3", n)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

func TestOutcomeWriteRetried_AskPause(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 0)
	attempts := failStatusWrites(t, store, task.ID, 2, "08006", models.TaskStatusPausedAwaitingInput)

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
	got := assertOutcome(t, store, task.ID, models.TaskStatusPausedAwaitingInput)
	if got.PendingQuestion != "which environment?" {
		t.Errorf("pending_question = %q", got.PendingQuestion)
	}
	if n := attempts(); n != 3 {
		t.Errorf("pause write attempts = %d, want 3", n)
	}
	// The person is asked once the park landed (TestAskPauseThatDidNotLand…
	// covers a park that did not).
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusProgress || !strings.Contains(ev.Message, "which environment?") {
		t.Errorf("notification = %+v, want one progress notification asking the question", ev)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// TestRefusedFreeFormSuccessIsRecordedNotRerun is the audit's reproduction: a
// success write the database refuses outright (here a CHECK constraint added
// at the commit boundary) left the free-form run `running`, and lease
// recovery re-queued it for a second run. A refusal no retry can fix is now
// recorded as a terminal error that says so.
func TestRefusedFreeFormSuccessIsRecordedNotRerun(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	task := seedOutcomeTask(t, store, 3)

	var calls atomic.Int32
	notif := &fakeNotifier{}
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{Notifier: notif})
	const constraint = "runner_test_refuse_success"
	pool.beforeSuccessCommit = func(*models.Task, uuid.UUID) {
		if _, err := store.DB().Conn().ExecContext(context.Background(),
			"ALTER TABLE tasks ADD CONSTRAINT "+constraint+" CHECK (status <> 'success') NOT VALID"); err != nil {
			t.Errorf("add constraint: %v", err)
		}
	}
	stop := runPool(t, pool)

	waitRunReturned(t, pool, &calls)
	if _, err := store.DB().Conn().ExecContext(t.Context(), "ALTER TABLE tasks DROP CONSTRAINT IF EXISTS "+constraint); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	got := assertOutcome(t, store, task.ID, models.TaskStatusError)
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "success could not be recorded") {
		t.Errorf("error_message = %v, want it to say the success could not be recorded", got.ErrorMessage)
	}
	if ev := waitOneNotification(t, notif); ev.Status != notify.StatusFailure {
		t.Errorf("notification = %+v, want one failure (the row says error)", ev)
	}
	assertNotRunAgain(t, store, task.ID)
	stop()
	if n := calls.Load(); n != 1 {
		t.Fatalf("runner calls = %d, want 1", n)
	}
}

// TestCrashedRunIsStillRecovered is the control: a run the process died in
// the middle of (it never ended, so it never stamped finished_run_id) is
// recovered exactly as before — re-queued while its retry budget lasts, and
// re-run as attempt 2 (what drives the "[retry] attempt N" preamble), or
// dead-lettered by the crash-loop guard once the budget is spent.
func TestCrashedRunIsStillRecovered(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	crash := func(maxRetries int) *models.Task {
		task := seedOutcomeTask(t, store, maxRetries)
		token := uuid.New() // the dead process's claim
		claimed, err := store.ClaimNextPendingTask(ctx, token.String())
		if err != nil || claimed == nil || claimed.ID != task.ID {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if _, err := store.UpdateTaskStatusAtomic(task.ID, token, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
			t.Fatalf("running: %v", err)
		}
		if _, err := store.DB().Conn().ExecContext(ctx,
			`UPDATE tasks SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, task.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		return task
	}
	retried := crash(1)
	looping := crash(0)

	if n, err := store.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, %v; want the within-budget crash re-queued", n, err)
	}
	if got, _ := store.GetTask(looping.ID); got.Status != models.TaskStatusDeadLettered ||
		got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "crash-loop guard") {
		t.Fatalf("budget-spent crash: %+v, want the crash-loop guard's dead-letter", got)
	}

	var attempts []int
	var mu sync.Mutex
	var calls atomic.Int32
	pool := newOutcomeTestPool(store, TaskRunnerFunc(func(_ context.Context, tk *models.Task) (*models.LogSession, error) {
		mu.Lock()
		attempts = append(attempts, tk.AttemptCount)
		mu.Unlock()
		calls.Add(1)
		return &models.LogSession{ID: "s-" + tk.ID.String()}, nil
	}), Config{})
	stop := runPool(t, pool)
	waitFor(t, 5*time.Second, func() bool {
		got, err := store.GetTask(retried.ID)
		return err == nil && got.Status == models.TaskStatusSuccess
	})
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("re-run attempts = %v, want one run as attempt_count 1", attempts)
	}
}
