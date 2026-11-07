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
	if len(s.chatWake) != 1 {
		t.Fatal("a wake request within one tick interval was dropped")
	}
	s.WakeChatSchedulesAt(time.Now().Add(11 * time.Minute))
	if len(s.chatWake) != 1 {
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
