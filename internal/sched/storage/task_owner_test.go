package storage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedOccurrence inserts one occurrence of a recurring job the way the spawn
// does — live, with its predecessor and lineage stamped by the insert (both
// columns are insert-only) — then moves it to status.
func seedOccurrence(t *testing.T, store *Storage, owner uuid.UUID, lineage uuid.UUID, prev *uuid.UUID, status models.TaskStatus) *models.Task {
	t.Helper()
	id := uuid.New()
	if lineage == uuid.Nil {
		lineage = id
	}
	future := time.Now().Add(time.Hour).UTC()
	task := &models.Task{
		ID:                   id,
		LineageID:            lineage,
		Prompt:               "refresh the dashboard",
		Status:               models.TaskStatusScheduled,
		ScheduledFor:         &future,
		Priority:             models.PriorityNormal,
		Recurrence:           "@daily",
		Timezone:             "UTC",
		CreatedAt:            time.Now().UTC(),
		CreatedBy:            &owner,
		PreviousOccurrenceID: prev,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if status != models.TaskStatusScheduled {
		completed := time.Now().Add(-10 * time.Minute).UTC()
		task.Status = status
		task.CompletedAt = &completed
		if err := store.DB().UpdateTask(context.Background(), task); err != nil {
			t.Fatalf("UpdateTask(%s): %v", status, err)
		}
	}
	return task
}

func ownerOf(t *testing.T, store *Storage, id uuid.UUID) uuid.UUID {
	t.Helper()
	task, err := store.GetTask(id)
	if err != nil || task == nil || task.CreatedBy == nil {
		t.Fatalf("GetTask(%s) = %+v, %v", id, task, err)
	}
	return *task.CreatedBy
}

// TestSetTaskOwnerFollowsTheChain: the reassignment rewrites the task, the
// unfinished later occurrences of its own recurrence chain, and the chain's
// last occurrence. Finished occurrences in between are history and keep their
// owner, and a clone that shares the task's lineage_id — a different job
// copied from it — is not touched: the chain is followed through
// previous_occurrence_id, not lineage.
func TestSetTaskOwnerFollowsTheChain(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	first := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusSuccess)
	second := seedOccurrence(t, store, oldOwner, first.LineageID, &first.ID, models.TaskStatusSuccess)
	live := seedOccurrence(t, store, oldOwner, first.LineageID, &second.ID, models.TaskStatusScheduled)
	clone := seedOccurrence(t, store, oldOwner, first.LineageID, nil, models.TaskStatusScheduled)

	updated, err := store.SetTaskOwner(ctx, first.ID, newOwner, nil)
	if err != nil {
		t.Fatalf("SetTaskOwner: %v", err)
	}
	if !slices.Equal(updated, []uuid.UUID{first.ID, live.ID}) {
		t.Fatalf("updated = %v, want [%s %s] (the task first)", updated, first.ID, live.ID)
	}
	for id, want := range map[uuid.UUID]uuid.UUID{first.ID: newOwner, live.ID: newOwner, second.ID: oldOwner, clone.ID: oldOwner} {
		if got := ownerOf(t, store, id); got != want {
			t.Errorf("owner of %s = %s, want %s", id, got, want)
		}
	}

	if _, err := store.SetTaskOwner(ctx, uuid.New(), newOwner, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing task = %v, want sql.ErrNoRows", err)
	}
}

// TestSetTaskOwnerReachesAFinishedLastOccurrence: a chain whose last
// occurrence has finished continues from that row — its pending spawn, or a
// replay of a parked dead-letter (the 2026-10-26 copies parked after two
// dead-letters) — so the last row takes the new owner even though it is
// finished. Without that, a replay would resume the chain under the old owner.
func TestSetTaskOwnerReachesAFinishedLastOccurrence(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	first := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusDeadLettered)
	parked := seedOccurrence(t, store, oldOwner, first.LineageID, &first.ID, models.TaskStatusDeadLettered)

	updated, err := store.SetTaskOwner(ctx, first.ID, newOwner, nil)
	if err != nil {
		t.Fatalf("SetTaskOwner: %v", err)
	}
	if !slices.Equal(updated, []uuid.UUID{first.ID, parked.ID}) {
		t.Fatalf("updated = %v, want [%s %s]", updated, first.ID, parked.ID)
	}
	if got := ownerOf(t, store, parked.ID); got != newOwner {
		t.Fatalf("last occurrence owner = %s, want the new owner %s", got, newOwner)
	}
}

// TestSetTaskOwnerCheckSeesEveryRow: the caller's check runs on every row the
// reassignment would rewrite, each as it would read afterwards (CreatedBy =
// the new owner) — including a later occurrence whose selection differs from
// the task's — and one refusal writes nothing.
func TestSetTaskOwnerCheckSeesEveryRow(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	first := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusSuccess)
	next := seedOccurrence(t, store, oldOwner, first.LineageID, &first.ID, models.TaskStatusScheduled)
	next.MCPSelection = models.MCPSelection{{Server: "edited-in"}}
	if err := store.DB().UpdateTask(ctx, next); err != nil {
		t.Fatalf("edit next occurrence: %v", err)
	}

	var seen []uuid.UUID
	refuse := errors.New("edited-in does not resolve for the new owner")
	_, err := store.SetTaskOwner(ctx, first.ID, newOwner, func(task *models.Task) error {
		if task.CreatedBy == nil || *task.CreatedBy != newOwner {
			t.Errorf("check saw owner %v for %s, want the new owner", task.CreatedBy, task.ID)
		}
		seen = append(seen, task.ID)
		if len(task.MCPSelection) > 0 && task.MCPSelection[0].Server == "edited-in" {
			return refuse
		}
		return nil
	})
	var refusal *TaskOwnerCheckError
	if !errors.As(err, &refusal) || refusal.TaskID != next.ID || !errors.Is(err, refuse) {
		t.Fatalf("err = %v, want a TaskOwnerCheckError for %s", err, next.ID)
	}
	if !slices.Equal(seen, []uuid.UUID{first.ID, next.ID}) {
		t.Fatalf("check saw %v, want [%s %s]", seen, first.ID, next.ID)
	}
	for _, id := range []uuid.UUID{first.ID, next.ID} {
		if got := ownerOf(t, store, id); got != oldOwner {
			t.Fatalf("a refused reassignment wrote owner %s on %s", got, id)
		}
	}
}

// TestSetTaskOwnerReachesTheNextSpawn: a recurrence spawn copies its owner
// from the completing occurrence, so the occurrence spawned after a
// reassignment belongs to the new owner.
func TestSetTaskOwnerReachesTheNextSpawn(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	// A finished occurrence whose spawn has not landed yet.
	done := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusSuccess)
	if _, err := store.SetTaskOwner(ctx, done.ID, newOwner, nil); err != nil {
		t.Fatalf("SetTaskOwner: %v", err)
	}
	if repaired, err := store.ReconcileRecurrences(ctx); err != nil || repaired != 1 {
		t.Fatalf("ReconcileRecurrences = %d, %v; want one spawn", repaired, err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{done.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want 1", len(succ))
	}
	if succ[0].CreatedBy == nil || *succ[0].CreatedBy != newOwner {
		t.Fatalf("next occurrence owner = %v, want the new owner %s", succ[0].CreatedBy, newOwner)
	}
	if succ[0].PreviousOccurrenceID == nil || *succ[0].PreviousOccurrenceID != done.ID {
		t.Fatalf("successor previous_occurrence_id = %v, want %s", succ[0].PreviousOccurrenceID, done.ID)
	}
}
