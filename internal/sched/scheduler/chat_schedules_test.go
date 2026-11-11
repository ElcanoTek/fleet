package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// storeDeliverer delivers fires through the real chat store, as the chat
// HTTP server does, and records what was settled.
type storeDeliverer struct {
	chat *store.Store

	mu      sync.Mutex
	settled []models.ChatScheduleDelivery
	kicks   int
}

func (d *storeDeliverer) DeliverChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire) (models.ChatScheduleDelivery, error) {
	return d.chat.DeliverChatScheduleFire(ctx, fire, 20)
}

func (d *storeDeliverer) ChatScheduleFireSettled(_ context.Context, _ models.ChatScheduleFire, del models.ChatScheduleDelivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settled = append(d.settled, del)
}

func (d *storeDeliverer) KickQueuedChatScheduleTurns(context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kicks++
}

func (d *storeDeliverer) settledCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.settled)
}

// newChatScheduleFleet is one database with both stores, a scheduler wired
// to the chat store, and a conversation.
func newChatScheduleFleet(t *testing.T) (*Scheduler, *store.Store, *storeDeliverer, string) {
	t.Helper()
	fdb := fleetdb.New(t)
	chat := fdb.Chat(t)
	st := storage.New()
	st.SetDatabase(fdb.SchedDB(t))
	st.SetTimezone("UTC")
	s := New(st, "UTC")
	d := &storeDeliverer{chat: chat}
	s.SetChatScheduleDeliverer(d)
	conv, err := chat.CreateConversation(context.Background(), "owner@elcanotek.com", "Patrol chat", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return s, chat, d, conv.ID
}

// TestChatSchedules_TickFiresDueSchedule: the scheduler's tick phase claims a
// due chat patrol, the chat store queues its turn, and the settle advances
// the schedule and records the run — one engine end to end.
func TestChatSchedules_TickFiresDueSchedule(t *testing.T) {
	t.Parallel()
	s, chat, d, convID := newChatScheduleFleet(t)
	// Pin one second before a 10-minute boundary so neither the post-settle
	// NextRunAt check nor the follow-up pass races a wall-clock rollover into
	// 19:40:00Z.
	fixedNow := time.Date(2026, 11, 11, 19, 39, 59, 0, time.UTC)
	s.clock = func() time.Time { return fixedNow }
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Title: "Sweep", Prompt: "Check.",
		CronExpr: "*/10 * * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET next_run_at = $1 WHERE id = $2`, fixedNow.Add(-time.Minute).Unix(), sc.ID); err != nil {
		t.Fatal(err)
	}

	s.runChatSchedulesAndWait()

	if d.settledCount() != 1 || d.settled[0].Outcome != models.ChatScheduleQueued || d.settled[0].InputID == "" {
		t.Fatalf("settled = %+v", d.settled)
	}
	if d.kicks < 1 {
		t.Errorf("queued patrol turns were not kicked (kicks = %d)", d.kicks)
	}
	wantNext := time.Date(2026, 11, 11, 19, 40, 0, 0, time.UTC).Unix()
	list, err := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, false)
	if err != nil || len(list) != 1 || list[0].RunCount != 1 || list[0].NextRunAt != wantNext {
		t.Fatalf("schedule after the tick = %+v, %v (want next_run_at %d)", list, err, wantNext)
	}
	runs, err := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].InputID != d.settled[0].InputID {
		t.Fatalf("run history = %+v, %v", runs, err)
	}

	// The next tick finds the patrol turn in flight and nothing due.
	s.runChatSchedulesAndWait()
	if d.settledCount() != 1 {
		t.Fatalf("a second tick fired again: %+v", d.settled)
	}
}

// TestChatSchedules_RunNow: Run now goes through the scheduler; a second
// click while the turn is queued coalesces onto it without counting a run.
func TestChatSchedules_RunNow(t *testing.T) {
	t.Parallel()
	s, chat, _, convID := newChatScheduleFleet(t)
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RunChatScheduleNow(ctx, sc.ID)
	if err != nil || first.Outcome != models.ChatScheduleQueued {
		t.Fatalf("run now = %+v, %v", first, err)
	}
	if _, err := s.RunChatScheduleNow(ctx, sc.ID); !errors.Is(err, models.ErrChatScheduleFiring) {
		t.Fatalf("second run now while the first's turn is queued: err = %v, want ErrChatScheduleFiring", err)
	}
	list, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, false)
	if len(list) != 1 || list[0].RunCount != 1 {
		t.Fatalf("schedule = %+v; want one counted run", list)
	}
	if runs, _ := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 10); len(runs) != 1 {
		t.Fatalf("run history = %+v; want only the counted run", runs)
	}
}

// TestChatSchedules_ConcurrentRunNow is the rehearsal's race: just after a
// Run now whose turn has finished, two Run nows at once. Exactly one runs
// (queued) and the other is refused with ErrChatScheduleFiring; one turn, one
// counted run and one history row are added, and no history row carries a run
// number the next fire will take.
func TestChatSchedules_ConcurrentRunNow(t *testing.T) {
	t.Parallel()
	s, chat, _, convID := newChatScheduleFleet(t)
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RunChatScheduleNow(ctx, sc.ID)
	if err != nil || first.Outcome != models.ChatScheduleQueued {
		t.Fatalf("run now = %+v, %v", first, err)
	}
	// The first turn ran to completion.
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat.chat_input_queue SET state = 'completed' WHERE id = $1`, first.InputID); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 5; round++ {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			oks  int
			fire int
		)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := s.RunChatScheduleNow(ctx, sc.ID)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && d.Outcome == models.ChatScheduleQueued:
					oks++
				case errors.Is(err, models.ErrChatScheduleFiring):
					fire++
				default:
					t.Errorf("round %d: run now = %+v, %v", round, d, err)
				}
			}()
		}
		wg.Wait()
		if oks != 1 || fire != 1 {
			t.Fatalf("round %d: %d queued and %d refused, want exactly one of each", round, oks, fire)
		}
		if _, err := s.storage.DB().Conn().Exec(`UPDATE chat.chat_input_queue SET state = 'completed' WHERE client_input_id LIKE $1`,
			store.ChatPatrolInputKeyPrefix+sc.ID+":%"); err != nil {
			t.Fatal(err)
		}
	}

	all, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, true)
	if len(all) != 1 || all[0].RunCount != 6 {
		t.Fatalf("schedule = %+v; want 6 counted runs", all)
	}
	runs, err := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 50)
	if err != nil || len(runs) != 6 {
		t.Fatalf("run history = %+v, %v; want 6 rows", runs, err)
	}
	seen := map[int]bool{}
	for _, r := range runs {
		if r.Outcome != models.ChatScheduleQueued || r.RunNumber < 1 || r.RunNumber > 6 || seen[r.RunNumber] {
			t.Fatalf("run history = %+v; want runs 1..6, each once, all queued", runs)
		}
		seen[r.RunNumber] = true
	}
}

// TestChatSchedules_WakeFiresBetweenTicks: a short timer fires at its due
// time through the running loop's wake-up, long before the next tick.
func TestChatSchedules_WakeFiresBetweenTicks(t *testing.T) {
	t.Parallel()
	s, chat, d, convID := newChatScheduleFleet(t)
	ctx := context.Background()
	s.tickInterval = time.Hour
	s.Start()
	t.Cleanup(s.Stop)

	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Ping.", DurationSeconds: store.MinChatTimerDurationSeconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Due now (as if created DurationSeconds ago), then the wake-up the chat
	// server asks for on create.
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET next_run_at = $1 WHERE id = $2`, time.Now().Unix(), sc.ID); err != nil {
		t.Fatal(err)
	}
	s.WakeChatSchedulesAt(time.Now())

	deadline := time.Now().Add(10 * time.Second)
	for d.settledCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.settledCount() != 1 || d.settled[0].Outcome != models.ChatScheduleQueued {
		t.Fatalf("timer did not fire on its wake-up: %+v", d.settled)
	}
}

// cancelAfterDeliver wraps the store deliverer and ends the caller's context
// right after the delivery commits — an HTTP client that disconnected while
// Run now was in flight (the handler passes r.Context()).
type cancelAfterDeliver struct {
	*storeDeliverer
	cancel context.CancelFunc
}

func (c cancelAfterDeliver) DeliverChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire) (models.ChatScheduleDelivery, error) {
	d, err := c.storeDeliverer.DeliverChatScheduleFire(ctx, fire)
	c.cancel()
	return d, err
}

// TestChatSchedules_RunNowCallerGoneStillSettles: the Run now path the HTTP
// handler takes, against the real chat store: the caller's context ending
// after the delivery must not leave the run unsettled or the lease held.
func TestChatSchedules_RunNowCallerGoneStillSettles(t *testing.T) {
	t.Parallel()
	s, chat, d, convID := newChatScheduleFleet(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.SetChatScheduleDeliverer(cancelAfterDeliver{storeDeliverer: d, cancel: cancel})
	sc, err := chat.CreateChatSchedule(context.Background(), store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RunChatScheduleNow(ctx, sc.ID)
	if err != nil || got.Outcome != models.ChatScheduleQueued {
		t.Fatalf("run now = %+v, %v", got, err)
	}
	var leased bool
	if err := s.storage.DB().Conn().QueryRow(`SELECT lease_owner IS NOT NULL FROM chat_schedules WHERE id = $1`, sc.ID).Scan(&leased); err != nil || leased {
		t.Fatalf("lease still held after Run now (err %v)", err)
	}
	list, _ := chat.ListChatSchedules(context.Background(), "owner@elcanotek.com", convID, false)
	if len(list) != 1 || list[0].RunCount != 1 {
		t.Fatalf("schedule = %+v; want the run counted", list)
	}
}

// TestChatSchedules_UnsettledRunNowIsNotDuplicated: a Run now delivered but
// never settled (the process died) cannot be repeated as a new fire; the
// recovery pass re-drives it by the same key, so there is one turn and one
// counted run, even on a paused schedule.
func TestChatSchedules_UnsettledRunNowIsNotDuplicated(t *testing.T) {
	t.Parallel()
	s, chat, _, convID := newChatScheduleFleet(t)
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := store.ChatScheduleStatusPaused
	if _, err := chat.UpdateChatSchedule(ctx, "owner@elcanotek.com", convID, sc.ID, store.ChatSchedulePatch{Status: &st}); err != nil {
		t.Fatal(err)
	}
	fire, err := s.storage.DB().ClaimChatScheduleNow(ctx, sc.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first, err := chat.DeliverChatScheduleFire(ctx, fire, 20)
	if err != nil || first.Outcome != models.ChatScheduleQueued {
		t.Fatalf("deliver = %+v, %v", first, err)
	}
	// ...and the process dies before the settle.
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, sc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunChatScheduleNow(ctx, sc.ID); !errors.Is(err, models.ErrChatScheduleFiring) {
		t.Fatalf("Run now over the unsettled fire: err = %v, want ErrChatScheduleFiring", err)
	}
	s.runChatSchedulesAndWait()

	var turns int
	if err := s.storage.DB().Conn().QueryRow(`SELECT count(*) FROM chat.chat_input_queue WHERE client_input_id LIKE $1`,
		store.ChatPatrolInputKeyPrefix+sc.ID+":%").Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("patrol turns = %d (err %v), want 1", turns, err)
	}
	all, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, true)
	if len(all) != 1 || all[0].RunCount != 1 || all[0].Status != store.ChatScheduleStatusPaused {
		t.Fatalf("schedule = %+v; want paused with one counted run", all)
	}
	runs, err := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].InputID != first.InputID || runs[0].Trigger != models.ChatScheduleTriggerManual {
		t.Fatalf("run history = %+v, %v", runs, err)
	}
}

// fillInputQueue queues n user inputs in the conversation (the deliverer's
// maxPending is 20).
func fillInputQueue(t *testing.T, s *Scheduler, convID string, n int) {
	t.Helper()
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		if _, err := s.storage.DB().Conn().Exec(`INSERT INTO chat.chat_input_queue
			(id, conversation_id, user_email, client_input_id, message, attachments, mode, state, position, created_at, updated_at)
			VALUES ($1, $2, 'owner@elcanotek.com', $1, 'hi', '[]', 'queued', 'queued', $3, $4, $4)`,
			fmt.Sprintf("full-%s-%02d", convID, i), convID, i+1, now); err != nil {
			t.Fatal(err)
		}
	}
}

func drainInputQueue(t *testing.T, s *Scheduler, convID string) {
	t.Helper()
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat.chat_input_queue SET state = 'completed' WHERE conversation_id = $1`, convID); err != nil {
		t.Fatal(err)
	}
}

func patrolTurns(t *testing.T, s *Scheduler, scheduleID string) int {
	t.Helper()
	var n int
	if err := s.storage.DB().Conn().QueryRow(`SELECT count(*) FROM chat.chat_input_queue WHERE client_input_id LIKE $1`,
		store.ChatPatrolInputKeyPrefix+scheduleID+":%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestChatSchedules_RunNowOnFullQueueIsRefused: a Run now on a conversation
// whose input queue is full is refused (ErrChatScheduleQueueFull, a 429) and
// ended — once the queue drains, no pass runs it.
func TestChatSchedules_RunNowOnFullQueueIsRefused(t *testing.T) {
	t.Parallel()
	s, chat, _, convID := newChatScheduleFleet(t)
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	fillInputQueue(t, s, convID, 20)
	if _, err := s.RunChatScheduleNow(ctx, sc.ID); !errors.Is(err, models.ErrChatScheduleQueueFull) {
		t.Fatalf("Run now on a full queue: err = %v, want ErrChatScheduleQueueFull", err)
	}
	drainInputQueue(t, s, convID)
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET lease_expires_at = now() - interval '1 second' WHERE lease_owner IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	s.runChatSchedulesAndWait()
	if n := patrolTurns(t, s, sc.ID); n != 0 {
		t.Fatalf("the refused Run now queued %d turn(s) after the queue drained", n)
	}
	all, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, true)
	if len(all) != 1 || all[0].RunCount != 0 {
		t.Fatalf("schedule = %+v; want no counted run", all)
	}
}

// TestChatSchedules_ScheduledFireOnFullQueueSkipsItsSlot: a scheduled fire
// meeting a full queue does what the chat sweeper always did — the slot is
// skipped (next_run_at moves to the next cron slot), nothing is queued or
// counted, and nothing is retried when the queue drains.
func TestChatSchedules_ScheduledFireOnFullQueueSkipsItsSlot(t *testing.T) {
	t.Parallel()
	s, chat, _, convID := newChatScheduleFleet(t)
	fixedNow := time.Date(2026, 11, 11, 8, 59, 59, 0, time.UTC)
	s.clock = func() time.Time { return fixedNow }
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Prompt: "Check.", CronExpr: "0 9 * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET next_run_at = $1 WHERE id = $2`, fixedNow.Add(-time.Minute).Unix(), sc.ID); err != nil {
		t.Fatal(err)
	}
	fillInputQueue(t, s, convID, 20)
	s.runChatSchedulesAndWait()

	next, _ := models.NextChatScheduleRun("0 9 * * *", "UTC", fixedNow)
	all, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, true)
	if len(all) != 1 || all[0].RunCount != 0 || all[0].NextRunAt != next.Unix() {
		t.Fatalf("schedule = %+v; want no run counted and the slot moved to %d", all, next.Unix())
	}
	drainInputQueue(t, s, convID)
	s.runChatSchedulesAndWait()
	if n := patrolTurns(t, s, sc.ID); n != 0 {
		t.Fatalf("the skipped slot queued %d turn(s) after the queue drained", n)
	}
	runs, err := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != models.ChatScheduleBusy || runs[0].RunNumber != 0 {
		t.Fatalf("run history = %+v, %v; want one unnumbered busy row", runs, err)
	}
}
