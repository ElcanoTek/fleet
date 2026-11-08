package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// fakeDeliverer stands in for the chat server: it "queues" every fire except
// the schedules in fail (an error), and can run a hook after delivering (to
// model, say, the caller's context ending right after the commit).
type fakeDeliverer struct {
	mu        sync.Mutex
	fail      map[string]bool
	delivered map[string][]string // schedule -> fire ids delivered
	settled   []string
	after     func()
	deferAll  bool                     // the chat server is draining: every fire is deferred
	coalesce  bool                     // a patrol turn is in flight: a manual fire coalesces
	block     map[string]chan struct{} // delivery to these schedules waits on the channel
	delay     time.Duration            // every delivery takes this long
	queueFull bool                     // the conversation's input queue is full
}

func newFakeDeliverer() *fakeDeliverer {
	return &fakeDeliverer{fail: map[string]bool{}, delivered: map[string][]string{}}
}

func (d *fakeDeliverer) DeliverChatScheduleFire(_ context.Context, f models.ChatScheduleFire) (models.ChatScheduleDelivery, error) {
	d.mu.Lock()
	if d.fail[f.ScheduleID] {
		d.mu.Unlock()
		return models.ChatScheduleDelivery{}, errors.New("delivery failed")
	}
	if d.queueFull && f.Trigger == models.ChatScheduleTriggerManual {
		d.mu.Unlock()
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleRejected, Reason: "queue_full"}, nil
	}
	if d.coalesce && f.Trigger == models.ChatScheduleTriggerManual {
		d.mu.Unlock()
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleCoalesced, InputID: "in-flight"}, nil
	}
	if d.delay > 0 {
		dl := d.delay
		d.mu.Unlock()
		time.Sleep(dl)
		d.mu.Lock()
	}
	if ch := d.block[f.ScheduleID]; ch != nil {
		d.mu.Unlock()
		<-ch
		d.mu.Lock()
	}
	if d.deferAll {
		d.mu.Unlock()
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleDeferred}, nil
	}
	d.delivered[f.ScheduleID] = append(d.delivered[f.ScheduleID], f.FireID)
	after := d.after
	d.mu.Unlock()
	if after != nil {
		after()
	}
	return models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: "in-" + f.FireID}, nil
}

func (d *fakeDeliverer) ChatScheduleFireSettled(_ context.Context, f models.ChatScheduleFire, _ models.ChatScheduleDelivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settled = append(d.settled, f.ScheduleID)
}

func (d *fakeDeliverer) KickQueuedChatScheduleTurns(context.Context) {}

func (d *fakeDeliverer) deliveredTo(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.delivered[id])
}

func newEngineScheduler(t *testing.T) (*Scheduler, *fakeDeliverer) {
	t.Helper()
	st := storage.New()
	st.SetDatabase(fleetdb.SchedDB(t))
	st.SetTimezone("UTC")
	s := New(st, "UTC")
	d := newFakeDeliverer()
	s.SetChatScheduleDeliverer(d)
	return s, d
}

func addChatSchedule(t *testing.T, s *Scheduler, id, status string, nextRunAt int64) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := s.storage.DB().Conn().Exec(`INSERT INTO chat_schedules
		(id, conversation_id, user_email, title, prompt, cron_expr, timezone, status, next_run_at, created_at, updated_at)
		VALUES ($1, 'conv', 'u@x.com', 'T', 'P', '*/10 * * * *', 'UTC', $2, $3, $4, $4)`,
		id, status, nextRunAt, now); err != nil {
		t.Fatal(err)
	}
}

func runCountOf(t *testing.T, s *Scheduler, id string) (int, bool) {
	t.Helper()
	var (
		n     int
		lease *string
	)
	if err := s.storage.DB().Conn().QueryRow(`SELECT run_count, lease_owner FROM chat_schedules WHERE id = $1`, id).Scan(&n, &lease); err != nil {
		t.Fatal(err)
	}
	return n, lease != nil
}

// A full batch of schedules whose deliveries fail must not starve the
// healthy schedule behind them: one pass reaches it, and the failing ones are
// not claimed again on the next pass (they back off).
func TestChatSchedules_FailingBatchDoesNotStarve(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	past := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < chatScheduleBatch; i++ {
		id := fmt.Sprintf("bad-%02d", i)
		addChatSchedule(t, s, id, "active", past) // due first
		d.fail[id] = true
	}
	addChatSchedule(t, s, "healthy", "active", past+60)

	s.runChatSchedules()
	if d.deliveredTo("healthy") != 1 {
		t.Fatalf("healthy schedule behind %d failing ones was not fired", chatScheduleBatch)
	}
	if n, leased := runCountOf(t, s, "bad-00"); n != 0 || !leased {
		t.Fatalf("failing schedule: run count %d, leased %v; want 0 and held for its backoff", n, leased)
	}
}

// A backlog larger than the old four-batch cap drains in one pass.
func TestChatSchedules_LargeBacklogDrainsInOnePass(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	past := time.Now().Add(-time.Hour).Unix()
	const n = 4*chatScheduleBatch + 30
	for i := 0; i < n; i++ {
		addChatSchedule(t, s, fmt.Sprintf("s-%03d", i), "active", past)
	}
	s.runChatSchedules()
	d.mu.Lock()
	got := len(d.delivered)
	d.mu.Unlock()
	if got != n {
		t.Fatalf("one pass fired %d of %d due schedules", got, n)
	}
}

// With a tick longer than five minutes, a wake request six minutes out must
// be kept: no tick would otherwise re-arm it before it is due.
func TestChatSchedules_WakeHorizonFollowsTick(t *testing.T) {
	t.Parallel()
	s := New(nil, "UTC")
	s.tickInterval = 10 * time.Minute
	s.WakeChatSchedulesAt(time.Now().Add(6 * time.Minute))
	if s.takeChatWake().IsZero() {
		t.Fatal("a wake request within one tick interval was dropped")
	}
	s.WakeChatSchedulesAt(time.Now().Add(11 * time.Minute))
	if !s.takeChatWake().IsZero() {
		t.Fatal("a wake request beyond the tick interval was kept")
	}
}

// Run now settles even when the caller's context ends right after the
// delivery commits (an HTTP client that disconnected): the run counts and the
// lease is released.
func TestChatSchedules_RunNowSettlesAfterCallerCancel(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	addChatSchedule(t, s, "p", "paused", time.Now().Add(time.Hour).Unix())
	ctx, cancel := context.WithCancel(context.Background())
	d.after = cancel

	got, err := s.RunChatScheduleNow(ctx, "p")
	if err != nil || got.Outcome != models.ChatScheduleQueued {
		t.Fatalf("run now = %+v, %v", got, err)
	}
	if n, leased := runCountOf(t, s, "p"); n != 1 || leased {
		t.Fatalf("after run now: run count %d, leased %v; want 1, released", n, leased)
	}
}

// The loop runs a chat schedule pass as soon as it starts, so a schedule due
// across a restart does not wait a whole tick.
func TestChatSchedules_PassAtStartup(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = time.Hour
	addChatSchedule(t, s, "due", "active", time.Now().Add(-time.Minute).Unix())
	s.Start()
	t.Cleanup(s.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for d.deliveredTo("due") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.deliveredTo("due") != 1 {
		t.Fatal("the due schedule was not fired at startup")
	}
}

func expireChatLease(t *testing.T, s *Scheduler, id string) {
	t.Helper()
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// A Run now deferred because the chat server is draining is kept, not
// dropped: on a paused schedule nothing else would ever fire it. Once the
// server takes turns again the recovery pass delivers it — once, as the same
// fire — and a due fire deferred at shutdown is likewise delivered once.
func TestChatSchedules_DeferredFiresAreRetried(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	addChatSchedule(t, s, "paused", "paused", time.Now().Add(time.Hour).Unix())
	addChatSchedule(t, s, "due", "active", time.Now().Add(-time.Minute).Unix())
	d.mu.Lock()
	d.deferAll = true
	d.mu.Unlock()

	got, err := s.RunChatScheduleNow(context.Background(), "paused")
	if err != nil || got.Outcome != models.ChatScheduleDeferred {
		t.Fatalf("run now while draining = %+v, %v", got, err)
	}
	s.runChatSchedules()

	d.mu.Lock()
	d.deferAll = false
	d.mu.Unlock()
	expireChatLease(t, s, "paused")
	expireChatLease(t, s, "due")
	s.runChatSchedules()
	s.runChatSchedules()

	for _, id := range []string{"paused", "due"} {
		if n := d.deliveredTo(id); n != 1 {
			t.Errorf("%s: delivered %d times after the drain ended, want 1", id, n)
		}
		if n, leased := runCountOf(t, s, id); n != 1 || leased {
			t.Errorf("%s: run count %d, leased %v; want 1, released", id, n, leased)
		}
	}
}

// A failed delivery's retry is armed as a wake-up at its backoff's end, not
// left to the next tick.
func TestChatSchedules_FailedDeliveryArmsRetryWake(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = time.Hour
	addChatSchedule(t, s, "bad", "active", time.Now().Add(-time.Minute).Unix())
	d.fail["bad"] = true
	s.runChatSchedules()
	at := s.takeChatWake()
	if at.IsZero() {
		t.Fatal("no wake-up armed for the failed fire's retry")
	}
	if until := time.Until(at); until < 20*time.Second || until > 40*time.Second {
		t.Fatalf("retry wake in %s, want about the 30s backoff", until)
	}
}

// The daily maintenance runs off the loop: a cleanup that blocks must not
// delay a due chat schedule's wake-up.
func TestChatSchedules_MaintenanceDoesNotBlockWake(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = time.Hour
	s.retentionDays = 30
	s.firstCleanupIn = time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{})
	s.maintenance = func() {
		close(started)
		select {
		case <-release:
		case <-s.ctx.Done():
		}
	}
	s.Start()
	t.Cleanup(func() {
		close(release)
		s.Stop()
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not start")
	}

	addChatSchedule(t, s, "due", "active", time.Now().Unix())
	s.WakeChatSchedulesAt(time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for d.deliveredTo("due") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.deliveredTo("due") != 1 {
		t.Fatal("a blocked maintenance run held up a due chat schedule")
	}
}

// lockDueTask adds a due, ungated scheduled task and holds its row lock in
// another transaction (released at cleanup), so promoting it blocks.
func lockDueTask(t *testing.T, s *Scheduler) {
	t.Helper()
	task := &models.Task{ID: uuid.New(), Prompt: "locked", Status: models.TaskStatusScheduled, ScheduledFor: pastDue(), CreatedAt: time.Now().UTC()}
	if _, err := s.storage.AddTask(task); err != nil {
		t.Fatal(err)
	}
	tx, err := s.storage.DB().Conn().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var id string
	if err := tx.QueryRow(`SELECT id::text FROM tasks WHERE id = $1 FOR UPDATE`, task.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
}

// A scheduled task's row held locked by another transaction blocks its
// promotion; that must not delay a due chat fire (the chat schedules have a
// loop of their own).
func TestChatSchedules_TaskRowLockDoesNotDelayChatFire(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = 100 * time.Millisecond
	lockDueTask(t, s)
	s.Start()
	t.Cleanup(s.Stop)
	time.Sleep(400 * time.Millisecond) // a tick is now blocked on the lock

	addChatSchedule(t, s, "due", "active", time.Now().Unix())
	s.WakeChatSchedulesAt(time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for d.deliveredTo("due") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.deliveredTo("due") != 1 {
		t.Fatal("a task row lock blocking promotion held up a due chat fire")
	}
}

// Stop returns promptly — the task loop actually exits — while promotion is
// blocked on a row lock: the promotion runs under the scheduler's context.
func TestStop_ReturnsWhilePromotionBlockedOnRowLock(t *testing.T) {
	t.Parallel()
	s, _ := newEngineScheduler(t)
	s.tickInterval = 100 * time.Millisecond
	lockDueTask(t, s)
	s.Start()
	time.Sleep(400 * time.Millisecond)

	start := time.Now()
	s.Stop()
	select {
	case <-s.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the task loop is still blocked on the row lock after Stop")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Stop took %s", took)
	}
}

// A Run now that finds its schedule's patrol turn still in flight is refused
// (ErrChatScheduleFiring, a 409), not answered as a success, and leaves no
// run-history row.
func TestChatSchedules_RunNowOverInFlightTurnIsFiring(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	addChatSchedule(t, s, "p", "active", time.Now().Add(time.Hour).Unix())
	d.coalesce = true
	if _, err := s.RunChatScheduleNow(context.Background(), "p"); !errors.Is(err, models.ErrChatScheduleFiring) {
		t.Fatalf("Run now over an in-flight turn: err = %v, want ErrChatScheduleFiring", err)
	}
	if runs, _ := s.storage.DB().ListChatScheduleRuns(context.Background(), "p", 10); len(runs) != 0 {
		t.Fatalf("run history = %+v, want none", runs)
	}
	if n, leased := runCountOf(t, s, "p"); n != 0 || leased {
		t.Fatalf("run count %d, leased %v", n, leased)
	}
}

// A Run now the chat server rejects (a full input queue) is refused with
// ErrChatScheduleQueueFull and ENDED: no lease or fire is kept, nothing is
// recorded, and once the queue drains no pass runs it.
func TestChatSchedules_RejectedRunNowIsTerminal(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	addChatSchedule(t, s, "p", "paused", time.Now().Add(time.Hour).Unix())
	d.queueFull = true
	if _, err := s.RunChatScheduleNow(context.Background(), "p"); !errors.Is(err, models.ErrChatScheduleQueueFull) {
		t.Fatalf("Run now on a full queue: err = %v, want ErrChatScheduleQueueFull", err)
	}
	var fire *string
	if err := s.storage.DB().Conn().QueryRow(`SELECT fire_id FROM chat_schedules WHERE id = 'p'`).Scan(&fire); err != nil || fire != nil {
		t.Fatalf("rejected fire kept (fire_id %v, err %v)", fire, err)
	}
	d.mu.Lock()
	d.queueFull = false
	d.mu.Unlock()
	expireChatLease(t, s, "p")
	s.runChatSchedules()
	if n := d.deliveredTo("p"); n != 0 {
		t.Fatalf("the rejected Run now was delivered %d time(s) after the queue drained", n)
	}
	if n, leased := runCountOf(t, s, "p"); n != 0 || leased {
		t.Fatalf("run count %d, leased %v; want 0, released", n, leased)
	}
	if runs, _ := s.storage.DB().ListChatScheduleRuns(context.Background(), "p", 10); len(runs) != 0 {
		t.Fatalf("run history = %+v, want none", runs)
	}
}

// TestProcessScheduledTasks_LockedRowDoesNotBlockOthers: the earliest due
// task is held FOR UPDATE by another transaction for longer than a statement
// may take, across several ticks. Promotion must pass over it — every other
// due task, on later pages too, is promoted — and promote it once the lock is
// released; Stop still returns promptly.
func TestProcessScheduledTasks_LockedRowDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	s, _ := newEngineScheduler(t)
	s.tickInterval = 100 * time.Millisecond
	s.storeTimeout = 300 * time.Millisecond
	s.scheduledBatchSize = 2

	add := func(age time.Duration) uuid.UUID {
		t.Helper()
		at := time.Now().UTC().Add(-age)
		task := &models.Task{ID: uuid.New(), Prompt: "due", Status: models.TaskStatusScheduled, ScheduledFor: &at, CreatedAt: time.Now().UTC()}
		if _, err := s.storage.AddTask(task); err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	locked := add(time.Hour) // the earliest: first in the first page
	others := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		others = append(others, add(time.Duration(30-i)*time.Minute))
	}
	tx, err := s.storage.DB().Conn().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if err := tx.QueryRow(`SELECT id::text FROM tasks WHERE id = $1 FOR UPDATE`, locked).Scan(&id); err != nil {
		t.Fatal(err)
	}

	status := func(id uuid.UUID) models.TaskStatus {
		t.Helper()
		task, err := s.storage.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		return task.Status
	}
	s.Start()
	stopped := false
	defer func() {
		if !stopped {
			s.Stop()
		}
	}()
	time.Sleep(4 * s.storeTimeout) // several ticks, each longer than a statement may take
	for _, o := range others {
		if st := status(o); st != models.TaskStatusPending {
			t.Fatalf("task %s behind the locked row is %s, want pending", o, st)
		}
	}
	if st := status(locked); st != models.TaskStatusScheduled {
		t.Fatalf("locked task is %s while its lock is held, want scheduled", st)
	}

	_ = tx.Rollback()
	deadline := time.Now().Add(5 * time.Second)
	for status(locked) != models.TaskStatusPending && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if st := status(locked); st != models.TaskStatusPending {
		t.Fatalf("locked task is %s after its lock was released, want pending", st)
	}

	start := time.Now()
	s.Stop()
	stopped = true
	select {
	case <-s.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the task loop did not exit after Stop")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Stop took %s", took)
	}
}

// TestChatSchedules_EarlyWakeSurvivesABusyLoop: while the chat loop is stuck
// in a pass, a flood of late wake requests arrives, then one earlier one. The
// earlier one must not be lost: its schedule fires at its time, long before
// the next tick (an hour away).
func TestChatSchedules_EarlyWakeSurvivesABusyLoop(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = time.Hour
	release := make(chan struct{})
	d.block = map[string]chan struct{}{"blocker": release}
	addChatSchedule(t, s, "blocker", "active", time.Now().Add(-time.Minute).Unix())
	s.Start()
	t.Cleanup(s.Stop)

	// The startup pass is now stuck delivering "blocker".
	deadline := time.Now().Add(5 * time.Second)
	for {
		var leased bool
		if err := s.storage.DB().Conn().QueryRow(`SELECT lease_owner IS NOT NULL FROM chat_schedules WHERE id = 'blocker'`).Scan(&leased); err != nil {
			t.Fatal(err)
		}
		if leased {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the startup pass never claimed the blocking schedule")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 100; i++ {
		s.WakeChatSchedulesAt(time.Now().Add(4 * time.Minute))
	}
	due := time.Now().Add(1500 * time.Millisecond)
	addChatSchedule(t, s, "early", "active", due.Unix())
	s.WakeChatSchedulesAt(due)
	close(release)

	deadline = time.Now().Add(10 * time.Second)
	for d.deliveredTo("early") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.deliveredTo("early") != 1 {
		t.Fatal("the early wake was lost behind the late ones; its schedule waits for the next tick")
	}
}

// TestChatSchedules_BacklogBeyondOnePassDrainsWithoutWaitingForTick: a pass
// that runs out of time with schedules still due asks for the next pass at
// once, so a backlog larger than one pass can fire drains well before the
// next tick (an hour away) — and no fire it had claimed is stranded.
func TestChatSchedules_BacklogBeyondOnePassDrainsWithoutWaitingForTick(t *testing.T) {
	t.Parallel()
	s, d := newEngineScheduler(t)
	s.tickInterval = time.Hour
	s.chatPassTimeoutOverride = 300 * time.Millisecond
	d.delay = 20 * time.Millisecond // a batch of 25 takes ~500ms: past the pass deadline
	const n = 3*chatScheduleBatch + 5
	past := time.Now().Add(-time.Minute).Unix()
	for i := 0; i < n; i++ {
		addChatSchedule(t, s, fmt.Sprintf("b-%03d", i), "active", past)
	}
	s.Start()
	t.Cleanup(s.Stop)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := len(d.delivered)
		d.mu.Unlock()
		if got == n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.mu.Lock()
	got := len(d.delivered)
	d.mu.Unlock()
	if got != n {
		t.Fatalf("%d of %d due schedules fired; the rest wait for the next tick", got, n)
	}
	var leased int
	if err := s.storage.DB().Conn().QueryRow(`SELECT count(*) FROM chat_schedules WHERE lease_owner IS NOT NULL`).Scan(&leased); err != nil || leased != 0 {
		t.Fatalf("%d fire(s) left leased (err %v)", leased, err)
	}
}

// TestProcessScheduledTasks_RowCapAsksForAnotherPass: promotion that stops at
// its per-pass row cap with due tasks left runs again right away instead of
// leaving them to the next tick.
func TestProcessScheduledTasks_RowCapAsksForAnotherPass(t *testing.T) {
	t.Parallel()
	s, _ := newEngineScheduler(t)
	s.tickInterval = time.Hour
	s.scheduledBatchSize = 2
	s.maxScheduledRows = 2
	ids := make([]uuid.UUID, 0, 7)
	for i := 0; i < 7; i++ {
		at := time.Now().UTC().Add(-time.Duration(10-i) * time.Minute)
		task := &models.Task{ID: uuid.New(), Prompt: "due", Status: models.TaskStatusScheduled, ScheduledFor: &at, CreatedAt: time.Now().UTC()}
		if _, err := s.storage.AddTask(task); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	s.Start()
	t.Cleanup(s.Stop)
	s.ProcessScheduledTasks() // one capped pass, as a tick would run it

	deadline := time.Now().Add(5 * time.Second)
	for {
		pending := 0
		for _, id := range ids {
			task, err := s.storage.GetTask(id)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status == models.TaskStatusPending {
				pending++
			}
		}
		if pending == len(ids) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d due tasks promoted; the rest wait for the next tick", pending, len(ids))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
