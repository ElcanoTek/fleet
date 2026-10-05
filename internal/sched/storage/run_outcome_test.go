package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func newDailyTask(t *testing.T, store *Storage) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "refresh the dashboard",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
		MaxRetries: 1,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// TestConnectorOutageDeadLettersDoNotParkTheChain: a dead-letter recorded with
// run outcome connector_unavailable neither parks on its own nor counts as the
// predecessor of the next dead-letter — one vendor blip no longer stops a
// daily schedule until a human replays it.
func TestConnectorOutageDeadLettersDoNotParkTheChain(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	first := newDailyTask(t, store)
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(first.ID, owner); err != nil {
		t.Fatal(err)
	}
	dl, err := store.DeadLetterTaskWithOutcomeWithContext(ctx, first.ID, owner, "connector unavailable: server pages", 1, models.RunOutcomeConnectorUnavailable, "pages (DNS lookup failed (no such host))")
	if err != nil {
		t.Fatal(err)
	}
	if !dl.IsRunOutcome(models.RunOutcomeConnectorUnavailable) {
		t.Fatalf("returned task outcome = %v, want connector_unavailable", dl.RunOutcome)
	}
	if got, _ := store.GetTask(first.ID); !got.IsRunOutcome(models.RunOutcomeConnectorUnavailable) {
		t.Fatalf("persisted outcome = %v, want connector_unavailable", got.RunOutcome)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want 1", len(succ))
	}
	second := succ[0]
	if second.RunOutcome != nil || second.InfraRetryCount != 0 {
		t.Fatalf("a successor must not inherit the outcome or the infra count: %+v %d", second.RunOutcome, second.InfraRetryCount)
	}

	// A genuine dead-letter right after the outage: its predecessor does not
	// count, so the chain continues.
	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(second.ID, owner2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, second.ID, owner2, "non-retryable failure (terminal): boom", 1); err != nil {
		t.Fatal(err)
	}
	succ = successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors after outage then dead-letter = %d, want 1 — the outage must not count toward the breaker", len(succ))
	}
	third := succ[0]

	// And an outage right after a genuine dead-letter does not park either.
	owner3 := uuid.New()
	if _, err := store.leaseTaskToOwner(third.ID, owner3); err != nil {
		t.Fatal(err)
	}
	parked, err := store.DeadLetterTaskWithOutcomeWithContext(ctx, third.ID, owner3, "connector unavailable", 1, models.RunOutcomeConnectorUnavailable, "pages (connection refused)")
	if err != nil {
		t.Fatal(err)
	}
	if parked.RecurrenceParkedReason != nil {
		t.Fatalf("an outage dead-letter parked the chain: %s", *parked.RecurrenceParkedReason)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true, third.ID: true})); n != 1 {
		t.Fatalf("successors after dead-letter then outage = %d, want 1", n)
	}
}

// TestConnectorOutageParksAfterThreeConsecutiveOccurrences: the exemption is
// bounded. A connector that never comes back (a typo'd or decommissioned
// host) dead-letters the first two occurrences without parking, and the third
// consecutive outage dead-letter parks the chain with a reason that names the
// connector and the streak (ADR-0077).
func TestConnectorOutageParksAfterThreeConsecutiveOccurrences(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	const detail = "pages (DNS lookup failed (no such host))"

	current := newDailyTask(t, store)
	seen := map[uuid.UUID]bool{}
	for i := 1; i <= 3; i++ {
		owner := uuid.New()
		if _, err := store.leaseTaskToOwner(current.ID, owner); err != nil {
			t.Fatal(err)
		}
		dl, err := store.DeadLetterTaskWithOutcomeWithContext(ctx, current.ID, owner, "connector unavailable", 1, models.RunOutcomeConnectorUnavailable, detail)
		if err != nil {
			t.Fatal(err)
		}
		seen[current.ID] = true
		succ := successorsOf(t, store, seen)
		if i < 3 {
			if dl.RecurrenceParkedReason != nil || len(succ) != 1 {
				t.Fatalf("outage %d: parked=%v successors=%d, want no park and one successor", i, dl.RecurrenceParkedReason, len(succ))
			}
			current = succ[0]
			continue
		}
		if len(succ) != 0 {
			t.Fatalf("the third consecutive outage spawned %d successor(s), want the chain parked", len(succ))
		}
		if dl.RecurrenceParkedReason == nil ||
			!strings.HasPrefix(*dl.RecurrenceParkedReason, "A required connector has been unreachable for 3 consecutive occurrences: "+detail+".") ||
			!strings.Contains(*dl.RecurrenceParkedReason, "replay this occurrence to resume the schedule") {
			t.Fatalf("park reason = %v, want the connector and the streak named", dl.RecurrenceParkedReason)
		}
		got, _ := store.GetTask(current.ID)
		if got.RecurrenceParkedAt == nil || got.RecurrenceParkedReason == nil || *got.RecurrenceParkedReason != *dl.RecurrenceParkedReason {
			t.Fatalf("the park must be persisted with its reason: %+v", got)
		}
	}
}

// TestInfraRequeueKeepsTheRetryBudgetAndReplayResetsIt: a connector-outage
// re-run increments infra_retry_count, never attempt_count, and a replay
// clears it with the outcome.
func TestInfraRequeueKeepsTheRetryBudgetAndReplayResetsIt(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	task := newDailyTask(t, store)
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(task.ID, owner); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Add(5 * time.Minute)
	got, err := store.RequeueTaskForInfraRetryWithContext(ctx, task.ID, owner, when, "connector unavailable; re-running in 5m")
	if err != nil {
		t.Fatal(err)
	}
	if got.InfraRetryCount != 1 || got.AttemptCount != 0 || got.Status != models.TaskStatusScheduled {
		t.Fatalf("after infra requeue: infra=%d attempts=%d status=%s, want 1/0/scheduled", got.InfraRetryCount, got.AttemptCount, got.Status)
	}
	persisted, _ := store.GetTask(task.ID)
	if persisted.InfraRetryCount != 1 || persisted.AttemptCount != 0 {
		t.Fatalf("persisted infra=%d attempts=%d, want 1/0", persisted.InfraRetryCount, persisted.AttemptCount)
	}

	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(task.ID, owner2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeadLetterTaskWithOutcomeWithContext(ctx, task.ID, owner2, "connector unavailable", 1, models.RunOutcomeConnectorUnavailable, "pages (connection refused)"); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ReplayDeadLetteredTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.InfraRetryCount != 0 || replayed.RunOutcome != nil {
		t.Fatalf("replay kept infra=%d outcome=%v, want a fresh slate", replayed.InfraRetryCount, replayed.RunOutcome)
	}
	if persisted, _ := store.GetTask(task.ID); persisted.InfraRetryCount != 0 || persisted.RunOutcome != nil {
		t.Fatalf("persisted after replay: infra=%d outcome=%v", persisted.InfraRetryCount, persisted.RunOutcome)
	}
}

// TestBlockedSuccessPersistsOutcomeAndCountsTheStreak: a success carrying the
// blocked outcome persists it with its detail, an ordinary success clears it,
// and BlockedStreak walks the lineage.
func TestBlockedSuccessPersistsOutcomeAndCountsTheStreak(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	blocked := models.RunOutcomeBlocked
	detail := "outcome=source_unreachable: the source mailbox had no report for 2026-10-04"

	current := newDailyTask(t, store)
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		owner := uuid.New()
		if _, err := store.leaseTaskToOwner(current.ID, owner); err != nil {
			t.Fatal(err)
		}
		msg := "Blocked: no source"
		landed, err := store.UpdateTaskStatusAtomicWithContext(ctx, current.ID, owner, &models.StatusUpdate{
			TaskID: current.ID, Status: models.TaskStatusSuccess, Message: &msg,
			RunOutcome: &blocked, RunOutcomeDetail: &detail,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !landed.IsRunOutcome(models.RunOutcomeBlocked) || landed.RunOutcomeDetail == nil || *landed.RunOutcomeDetail != detail {
			t.Fatalf("landed outcome = %v / %v", landed.RunOutcome, landed.RunOutcomeDetail)
		}
		ids = append(ids, current.ID)
		if streak, err := store.BlockedStreak(ctx, current.ID, 3); err != nil || streak != i+1 {
			t.Fatalf("BlockedStreak after %d blocked runs = %d, %v", i+1, streak, err)
		}
		seen := map[uuid.UUID]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		succ := successorsOf(t, store, seen)
		if len(succ) != 1 {
			t.Fatalf("successors = %d, want 1", len(succ))
		}
		current = succ[0]
	}
	if streak, _ := store.BlockedStreak(ctx, ids[2], 2); streak != 2 {
		t.Fatalf("BlockedStreak is bounded by its limit: got %d, want 2", streak)
	}

	// An ordinary success ends the streak and records no outcome.
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(current.ID, owner); err != nil {
		t.Fatal(err)
	}
	landed, err := store.UpdateTaskStatusAtomicWithContext(ctx, current.ID, owner, &models.StatusUpdate{TaskID: current.ID, Status: models.TaskStatusSuccess})
	if err != nil {
		t.Fatal(err)
	}
	if landed.RunOutcome != nil || landed.RunOutcomeDetail != nil {
		t.Fatalf("an ordinary success recorded outcome %v", landed.RunOutcome)
	}
	if streak, _ := store.BlockedStreak(ctx, current.ID, 3); streak != 0 {
		t.Fatalf("BlockedStreak of an ordinary success = %d, want 0", streak)
	}
}
