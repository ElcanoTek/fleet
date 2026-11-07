package scheduler

import (
	"context"
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
	ctx := context.Background()
	sc, err := chat.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID: convID, UserEmail: "owner@elcanotek.com", Title: "Sweep", Prompt: "Check.",
		CronExpr: "*/10 * * * *", Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.storage.DB().Conn().Exec(`UPDATE chat_schedules SET next_run_at = $1 WHERE id = $2`, time.Now().Add(-time.Minute).Unix(), sc.ID); err != nil {
		t.Fatal(err)
	}

	s.runChatSchedules()

	if d.settledCount() != 1 || d.settled[0].Outcome != models.ChatScheduleQueued || d.settled[0].InputID == "" {
		t.Fatalf("settled = %+v", d.settled)
	}
	if d.kicks != 1 {
		t.Errorf("queued patrol turns were not kicked (kicks = %d)", d.kicks)
	}
	list, err := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, false)
	if err != nil || len(list) != 1 || list[0].RunCount != 1 || list[0].NextRunAt <= time.Now().Unix() {
		t.Fatalf("schedule after the tick = %+v, %v", list, err)
	}
	runs, err := s.storage.DB().ListChatScheduleRuns(ctx, sc.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].InputID != d.settled[0].InputID {
		t.Fatalf("run history = %+v, %v", runs, err)
	}

	// The next tick finds the patrol turn in flight and nothing due.
	s.runChatSchedules()
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
	second, err := s.RunChatScheduleNow(ctx, sc.ID)
	if err != nil || second.Outcome != models.ChatScheduleCoalesced || second.InputID != first.InputID {
		t.Fatalf("second run now = %+v, %v; want coalesced onto %s", second, err, first.InputID)
	}
	list, _ := chat.ListChatSchedules(ctx, "owner@elcanotek.com", convID, false)
	if len(list) != 1 || list[0].RunCount != 1 {
		t.Fatalf("schedule = %+v; want one counted run", list)
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
