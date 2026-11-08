package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// insertChatSchedule adds a definition row as the chat server would.
func insertChatSchedule(t *testing.T, db *Database, id, cronExpr string, durationSeconds int, status string, nextRunAt int64, runCount, maxRuns int) {
	t.Helper()
	now := time.Now().Unix()
	if _, err := db.conn.Exec(`INSERT INTO chat_schedules
		(id, conversation_id, user_email, title, prompt, cron_expr, timezone, duration_seconds,
		 status, next_run_at, run_count, max_runs, created_at, updated_at)
		VALUES ($1, 'conv-1', 'u@x.com', 'T', 'P', $2, 'UTC', $3, $4, $5, $6, $7, $8, $8)`,
		id, cronExpr, durationSeconds, status, nextRunAt, runCount, maxRuns, now); err != nil {
		t.Fatal(err)
	}
}

type chatScheduleState struct {
	status    string
	nextRunAt int64
	runCount  int
	lastRunAt *int64
	leased    bool
}

func chatScheduleStateOf(t *testing.T, db *Database, id string) chatScheduleState {
	t.Helper()
	var (
		s     chatScheduleState
		last  *int64
		lease *string
	)
	if err := db.conn.QueryRow(`SELECT status, next_run_at, run_count, last_run_at, lease_owner FROM chat_schedules WHERE id = $1`, id).
		Scan(&s.status, &s.nextRunAt, &s.runCount, &last, &lease); err != nil {
		t.Fatal(err)
	}
	s.lastRunAt, s.leased = last, lease != nil
	return s
}

// assertSettledStates checks TestChatScheduleClaimSettle's rows after their
// settles.
func assertSettledStates(t *testing.T, db *Database, now time.Time) {
	t.Helper()
	next, _ := models.NextChatScheduleRun("*/10 * * * *", "UTC", now)
	if s := chatScheduleStateOf(t, db, "patrol"); s.status != "active" || s.runCount != 3 || s.nextRunAt != next.Unix() || s.lastRunAt == nil || *s.lastRunAt != now.Unix() || s.leased {
		t.Errorf("patrol after queued = %+v", s)
	}
	if s := chatScheduleStateOf(t, db, "capped"); s.status != "completed" || s.runCount != 3 {
		t.Errorf("capped after its last run = %+v", s)
	}
	if s := chatScheduleStateOf(t, db, "timer"); s.status != "completed" || s.runCount != 1 {
		t.Errorf("timer after queued = %+v", s)
	}
	if s := chatScheduleStateOf(t, db, "busy-timer"); s.status != "active" || s.runCount != 0 || s.nextRunAt != now.Unix()+30 || s.leased {
		t.Errorf("busy timer = %+v", s)
	}
	if s := chatScheduleStateOf(t, db, "gone"); s.status != "cancelled" {
		t.Errorf("gone = %+v", s)
	}
}

func TestChatScheduleClaimSettle(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	past := now.Add(-time.Minute).Unix()

	insertChatSchedule(t, db, "patrol", "*/10 * * * *", 0, "active", past, 2, 0)
	insertChatSchedule(t, db, "capped", "*/10 * * * *", 0, "active", past, 2, 3)
	insertChatSchedule(t, db, "timer", "", 60, "active", past, 0, 1)
	insertChatSchedule(t, db, "busy-timer", "", 60, "active", past, 0, 1)
	insertChatSchedule(t, db, "gone", "*/10 * * * *", 0, "active", past, 0, 0)
	insertChatSchedule(t, db, "paused", "*/10 * * * *", 0, "paused", past, 0, 0)
	insertChatSchedule(t, db, "future", "*/10 * * * *", 0, "active", now.Add(time.Hour).Unix(), 0, 0)

	fires, err := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]models.ChatScheduleFire{}
	for _, f := range fires {
		byID[f.ScheduleID] = f
	}
	if len(fires) != 5 || byID["paused"].ScheduleID != "" || byID["future"].ScheduleID != "" {
		t.Fatalf("claimed %d fires %+v; want the 5 active due ones", len(fires), fires)
	}
	if byID["patrol"].RunNumber != 3 || byID["patrol"].LeaseToken == "" {
		t.Fatalf("patrol fire = %+v", byID["patrol"])
	}

	settle := func(id string, outcome models.ChatScheduleOutcome, input string) {
		t.Helper()
		ok, err := db.SettleChatScheduleFire(ctx, byID[id], models.ChatScheduleDelivery{Outcome: outcome, InputID: input}, now)
		if err != nil || !ok {
			t.Fatalf("settle %s = %v, %v", id, ok, err)
		}
	}
	settle("patrol", models.ChatScheduleQueued, "in-patrol")
	settle("capped", models.ChatScheduleQueued, "in-capped")
	settle("timer", models.ChatScheduleQueued, "in-timer")
	settle("busy-timer", models.ChatScheduleBusy, "")
	settle("gone", models.ChatScheduleGone, "")

	assertSettledStates(t, db, now)

	runs, err := db.ListChatScheduleRuns(ctx, "patrol", 10)
	if err != nil || len(runs) != 1 || runs[0].RunNumber != 3 || runs[0].Outcome != models.ChatScheduleQueued ||
		runs[0].InputID != "in-patrol" || runs[0].Trigger != models.ChatScheduleTriggerSchedule || runs[0].DueAt == nil {
		t.Fatalf("patrol history = %+v, %v", runs, err)
	}

	// Nothing left due; a settle without the lease is refused.
	if more, err := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil); err != nil || len(more) != 0 {
		t.Fatalf("second pass = %+v, %v", more, err)
	}
	if ok, err := db.SettleChatScheduleFire(ctx, byID["patrol"], models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued}, now); err != nil || ok {
		t.Fatalf("settle after release = %v, %v; want lease lost", ok, err)
	}
}

// expireLease makes every held lease look abandoned.
func expireLease(t *testing.T, db *Database) {
	t.Helper()
	if _, err := db.conn.Exec(`UPDATE chat_schedules SET lease_expires_at = now() - interval '1 second' WHERE lease_owner IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

// A fire whose claimer died (the lease expired before any settle) is not a
// new due fire: the due claim leaves it, and the recovery pass re-claims the
// SAME fire (same id and run number, so the same idempotency key) under a new
// token; the dead claim's late settle then loses to the new lease.
func TestChatScheduleExpiredLeaseIsReclaimed(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "active", now.Add(-time.Minute).Unix(), 4, 0)

	first, err := db.ClaimDueChatSchedules(ctx, now, time.Second, 25, nil)
	if err != nil || len(first) != 1 || first[0].FireID == "" {
		t.Fatalf("claim = %+v, %v", first, err)
	}
	expireLease(t, db)
	if again, err := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil); err != nil || len(again) != 0 {
		t.Fatalf("due claim took an in-flight fire: %+v, %v", again, err)
	}
	second, err := db.RecoverChatScheduleFires(ctx, now, time.Minute, 25)
	if err != nil || len(second) != 1 {
		t.Fatalf("recover = %+v, %v", second, err)
	}
	if second[0].FireID != first[0].FireID || second[0].RunNumber != first[0].RunNumber ||
		second[0].Trigger != models.ChatScheduleTriggerSchedule || second[0].LeaseToken == first[0].LeaseToken {
		t.Fatalf("recovered = %+v, first = %+v", second[0], first[0])
	}
	if ok, err := db.SettleChatScheduleFire(ctx, first[0], models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued}, now); err != nil || ok {
		t.Fatalf("stale settle = %v, %v; want refused", ok, err)
	}
	if ok, err := db.SettleChatScheduleFire(ctx, second[0], models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: "in"}, now); err != nil || !ok {
		t.Fatalf("settle = %v, %v", ok, err)
	}
	if s := chatScheduleStateOf(t, db, "p"); s.runCount != 5 || s.leased {
		t.Fatalf("after settle = %+v, want run count 5 (one run, not two), unleased", s)
	}
}

// A Run now whose settle never happened (the process died, or the caller's
// context did) is recovered even though the schedule is paused and not due,
// as the same manual fire; until then a second Run now is refused rather than
// minting a new fire (and a new idempotency key) for the same run.
func TestChatScheduleUnsettledManualFireIsRecovered(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "paused", now.Add(time.Hour).Unix(), 2, 0)

	first, err := db.ClaimChatScheduleNow(ctx, "p", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expireLease(t, db)
	if _, err := db.ClaimChatScheduleNow(ctx, "p", now, time.Minute); !errors.Is(err, ErrChatScheduleFiring) {
		t.Fatalf("Run now over an unsettled fire: err = %v, want ErrChatScheduleFiring", err)
	}
	rec, err := db.RecoverChatScheduleFires(ctx, now, time.Minute, 25)
	if err != nil || len(rec) != 1 {
		t.Fatalf("recover = %+v, %v", rec, err)
	}
	if rec[0].FireID != first.FireID || rec[0].Trigger != models.ChatScheduleTriggerManual || rec[0].RunNumber != 3 {
		t.Fatalf("recovered = %+v, want the manual fire %s as run 3", rec[0], first.FireID)
	}
	if ok, err := db.SettleChatScheduleFire(ctx, rec[0], models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: "in"}, now); err != nil || !ok {
		t.Fatalf("settle = %v, %v", ok, err)
	}
	if s := chatScheduleStateOf(t, db, "p"); s.status != "paused" || s.runCount != 3 || s.leased {
		t.Fatalf("after recovery = %+v", s)
	}
}

// A failed delivery keeps its fire, with the lease pushed out by a growing
// backoff: neither the due claim nor the recovery pass takes it until then,
// and the retry is the same fire.
func TestChatScheduleFailedDeliveryBacksOff(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "active", now.Add(-time.Minute).Unix(), 0, 0)

	fs, err := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil)
	if err != nil || len(fs) != 1 {
		t.Fatalf("claim = %+v, %v", fs, err)
	}
	ok, backoff, err := db.FailChatScheduleFire(ctx, fs[0])
	if err != nil || !ok || backoff != ChatScheduleFailureBackoff(1) {
		t.Fatalf("fail = %v, %s, %v", ok, backoff, err)
	}
	var left float64
	if err := db.conn.QueryRow(`SELECT EXTRACT(EPOCH FROM lease_expires_at - now()) FROM chat_schedules WHERE id = 'p'`).Scan(&left); err != nil || left < 20 {
		t.Fatalf("lease after a failure ends in %.0fs (err %v), want the backoff", left, err)
	}
	if again, _ := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil); len(again) != 0 {
		t.Fatalf("due claim retook a failed fire: %+v", again)
	}
	if rec, _ := db.RecoverChatScheduleFires(ctx, now, time.Minute, 25); len(rec) != 0 {
		t.Fatalf("recovery retook a failed fire before its backoff: %+v", rec)
	}
	expireLease(t, db)
	rec, err := db.RecoverChatScheduleFires(ctx, now, time.Minute, 25)
	if err != nil || len(rec) != 1 || rec[0].FireID != fs[0].FireID {
		t.Fatalf("retry = %+v, %v; want fire %s", rec, err, fs[0].FireID)
	}
	if _, backoff, _ := db.FailChatScheduleFire(ctx, rec[0]); backoff != ChatScheduleFailureBackoff(2) || backoff <= ChatScheduleFailureBackoff(1) {
		t.Fatalf("second failure backoff = %s", backoff)
	}
	if ChatScheduleFailureBackoff(100) != chatScheduleFailureBackoffMax {
		t.Fatalf("backoff is not capped: %s", ChatScheduleFailureBackoff(100))
	}
}

func TestChatScheduleRunNowClaim(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "paused", "*/10 * * * *", 0, "paused", now.Add(time.Hour).Unix(), 1, 0)
	insertChatSchedule(t, db, "done", "", 60, "completed", now.Unix(), 1, 1)

	f, err := db.ClaimChatScheduleNow(ctx, "paused", now, time.Minute)
	if err != nil || f.Trigger != models.ChatScheduleTriggerManual || f.RunNumber != 2 {
		t.Fatalf("run now on paused = %+v, %v", f, err)
	}
	if _, err := db.ClaimChatScheduleNow(ctx, "paused", now, time.Minute); !errors.Is(err, ErrChatScheduleFiring) {
		t.Fatalf("second run now while leased: err = %v", err)
	}
	if _, err := db.ClaimChatScheduleNow(ctx, "done", now, time.Minute); !errors.Is(err, ErrChatScheduleNotRunnable) {
		t.Fatalf("run now on completed: err = %v", err)
	}
	if _, err := db.ClaimChatScheduleNow(ctx, "nope", now, time.Minute); !errors.Is(err, ErrChatScheduleNotFound) {
		t.Fatalf("run now on missing: err = %v", err)
	}
	// A manual run on a paused patrol counts and keeps it paused.
	if ok, err := db.SettleChatScheduleFire(ctx, f, models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: "in"}, now); err != nil || !ok {
		t.Fatalf("settle = %v, %v", ok, err)
	}
	if s := chatScheduleStateOf(t, db, "paused"); s.status != "paused" || s.runCount != 2 || s.leased {
		t.Fatalf("after manual run = %+v", s)
	}
	if due, ok, err := db.NextChatScheduleDueAt(ctx, 0); err != nil || ok {
		t.Fatalf("next due with nothing active = %d, %v, %v", due, ok, err)
	}
	insertChatSchedule(t, db, "past", "*/10 * * * *", 0, "active", now.Add(-time.Minute).Unix(), 0, 0)
	insertChatSchedule(t, db, "soon", "*/10 * * * *", 0, "active", now.Add(time.Minute).Unix(), 0, 0)
	if due, ok, err := db.NextChatScheduleDueAt(ctx, now.Unix()); err != nil || !ok || due != now.Add(time.Minute).Unix() {
		t.Fatalf("next due = %d, %v, %v; want the future slot only", due, ok, err)
	}
}

func TestCleanupOldChatScheduleRuns(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "active", time.Now().Unix(), 0, 0)
	for i, age := range []int{400, 300, 200, 1} { // days
		if _, err := db.conn.Exec(`INSERT INTO chat_schedule_runs (schedule_id, run_number, trigger_kind, outcome, fired_at)
			VALUES ('p', $1, 'schedule', 'busy', now() - make_interval(days => $2))`, i+1, age); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.CleanupOldChatScheduleRuns(ctx, 0, 1); err != nil || n != 0 {
		t.Fatalf("retention off pruned %d, %v", n, err)
	}
	n, err := db.CleanupOldChatScheduleRuns(ctx, 90, 2)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v; want 2 (keep the newest 2)", n, err)
	}
}

// A deferred fire (the chat server was draining) is kept, with a short
// retry, and the recovery pass retries it as the same fire — even a manual
// one on a paused schedule; and the next-wake time includes that retry.
func TestChatScheduleDeferredFireIsKept(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "paused", now.Add(time.Hour).Unix(), 0, 0)

	f, err := db.ClaimChatScheduleNow(ctx, "p", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.SettleChatScheduleFire(ctx, f, models.ChatScheduleDelivery{Outcome: models.ChatScheduleDeferred}, now); err != nil || !ok {
		t.Fatalf("settle deferred = %v, %v", ok, err)
	}
	if s := chatScheduleStateOf(t, db, "p"); !s.leased || s.runCount != 0 {
		t.Fatalf("deferred fire = %+v; want kept (leased), not counted", s)
	}
	// The retry (10s), not the claim's one-minute lease or the hour-away slot.
	due, ok, err := db.NextChatScheduleDueAt(ctx, now.Unix())
	if err != nil || !ok || due > time.Now().Add(ChatScheduleDeferRetry+2*time.Second).Unix() {
		t.Fatalf("next wake = %d (now %d), %v, %v; want the deferred fire's retry", due, now.Unix(), ok, err)
	}
	expireLease(t, db)
	rec, err := db.RecoverChatScheduleFires(ctx, now, time.Minute, 25)
	if err != nil || len(rec) != 1 || rec[0].FireID != f.FireID || rec[0].Trigger != models.ChatScheduleTriggerManual {
		t.Fatalf("retry = %+v, %v; want manual fire %s", rec, err, f.FireID)
	}
}

// Only a counted run carries a run number in the history: a busy fire's row
// has none (it would otherwise claim the number the next counted run takes),
// and a coalesced Run now is not recorded at all.
func TestChatScheduleHistoryNumbersOnlyCountedRuns(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	ctx := context.Background()
	now := time.Now()
	insertChatSchedule(t, db, "p", "*/10 * * * *", 0, "active", now.Add(-time.Minute).Unix(), 0, 0)

	settle := func(f models.ChatScheduleFire, o models.ChatScheduleOutcome) {
		t.Helper()
		if ok, err := db.SettleChatScheduleFire(ctx, f, models.ChatScheduleDelivery{Outcome: o, InputID: "in-" + string(o)}, now); err != nil || !ok {
			t.Fatalf("settle %s = %v, %v", o, ok, err)
		}
	}
	fs, _ := db.ClaimDueChatSchedules(ctx, now, time.Minute, 25, nil)
	settle(fs[0], models.ChatScheduleBusy)
	m, err := db.ClaimChatScheduleNow(ctx, "p", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	settle(m, models.ChatScheduleCoalesced)
	m, err = db.ClaimChatScheduleNow(ctx, "p", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	settle(m, models.ChatScheduleQueued)

	runs, err := db.ListChatScheduleRuns(ctx, "p", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(runs))
	for _, r := range runs {
		got = append(got, fmt.Sprintf("%d/%s", r.RunNumber, r.Outcome))
	}
	if want := "[1/queued 0/busy]"; fmt.Sprint(got) != want {
		t.Fatalf("history = %v, want %s (busy unnumbered, coalesced unrecorded)", got, want)
	}
}
