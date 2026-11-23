package storage

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// recordSpend writes one iteration of costUSD for taskID, started now.
func recordSpend(t *testing.T, store *Storage, taskID uuid.UUID, costUSD float64) {
	t.Helper()
	if err := store.AddTaskIteration(context.Background(), &models.TaskIteration{
		ID: uuid.New(), TaskID: taskID, IterationNumber: 1, Status: models.IterationStatusPassed,
		StartedAt: time.Now().UTC().Add(-time.Minute), CostUSD: costUSD, PromptTokens: 10,
	}); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
}

// spendByUser is the usage report's per-user task spend over the last day.
// The owners here have no account row, so their buckets are keyed by id.
func spendByUser(t *testing.T, store *Storage) map[string]float64 {
	t.Helper()
	now := time.Now().UTC()
	rows, err := store.TaskUsage(context.Background(), now.Add(-24*time.Hour), now.Add(time.Hour), "user")
	if err != nil {
		t.Fatalf("TaskUsage: %v", err)
	}
	out := map[string]float64{}
	for _, r := range rows {
		out[r.Key] = r.TaskCostUSD
	}
	return out
}

func requireSpend(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	for key, w := range want {
		if math.Abs(got[key]-w) > 1e-9 {
			t.Errorf("spend of %q = %.4f, want %.4f (all: %v)", key, got[key], w, got)
		}
	}
}

// TestOwnerReassignmentMovesRecordedSpend: an admin owner reassignment moves
// the reassigned rows' recorded spend to the new owner in the usage report
// (and so in the budgets), as when those read the owner off the task row;
// finished occurrences it leaves alone keep theirs. Each iteration carries
// that attribution itself, so it stays with the new owner after the tasks
// are deleted.
func TestOwnerReassignmentMovesRecordedSpend(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	first := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusSuccess)
	second := seedOccurrence(t, store, oldOwner, first.LineageID, &first.ID, models.TaskStatusSuccess)
	live := seedOccurrence(t, store, oldOwner, first.LineageID, &second.ID, models.TaskStatusScheduled)
	recordSpend(t, store, first.ID, 1)
	recordSpend(t, store, second.ID, 2)
	recordSpend(t, store, live.ID, 4)
	requireSpend(t, spendByUser(t, store), map[string]float64{oldOwner.String(): 7})

	// first and live are rewritten; second is history in between.
	if _, err := store.SetTaskOwner(ctx, first.ID, newOwner, nil); err != nil {
		t.Fatalf("SetTaskOwner: %v", err)
	}
	want := map[string]float64{oldOwner.String(): 2, newOwner.String(): 5}
	requireSpend(t, spendByUser(t, store), want)

	for _, id := range []uuid.UUID{first.ID, second.ID, live.ID} {
		if deleted, err := store.DeleteTask(ctx, id); err != nil || !deleted {
			t.Fatalf("DeleteTask(%s) = %v, %v", id, deleted, err)
		}
	}
	requireSpend(t, spendByUser(t, store), want)
}

// TestLineageOwnerAttributesRecordedSpend: `fleet sched task set-owner
// --lineage` gives unowned historical occurrences an owner, and their
// recorded spend moves out of the unattributed bucket with them.
func TestLineageOwnerAttributesRecordedSpend(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := uuid.New()

	completed := time.Now().Add(-time.Hour).UTC()
	hist := &models.Task{ID: uuid.New(), Prompt: "daily report", Status: models.TaskStatusSuccess,
		Priority: models.PriorityNormal, Recurrence: "@daily", Timezone: "UTC",
		CreatedAt: completed.Add(-time.Hour), CompletedAt: &completed}
	hist.LineageID = hist.ID
	if _, err := store.AddTask(hist); err != nil {
		t.Fatalf("AddTask(hist): %v", err)
	}
	recordSpend(t, store, hist.ID, 3)
	requireSpend(t, spendByUser(t, store), map[string]float64{"": 3})

	future := time.Now().Add(time.Hour).UTC()
	live := &models.Task{ID: uuid.New(), LineageID: hist.ID, Prompt: "daily report", Status: models.TaskStatusScheduled,
		ScheduledFor: &future, Priority: models.PriorityNormal, Recurrence: "@daily", Timezone: "UTC",
		CreatedAt: time.Now().UTC(), PreviousOccurrenceID: &hist.ID}
	if _, err := store.AddTask(live); err != nil {
		t.Fatalf("AddTask(live): %v", err)
	}

	if _, err := store.SetTaskLineageOwner(ctx, live.ID, owner); err != nil {
		t.Fatalf("SetTaskLineageOwner: %v", err)
	}
	got := spendByUser(t, store)
	requireSpend(t, got, map[string]float64{owner.String(): 3})
	if _, ok := got[""]; ok {
		t.Errorf("the unattributed bucket should be empty after the lineage owner: %v", got)
	}
}
