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

// TestSetTaskOwnerFollowsTheChain: the reassignment rewrites the task and the
// UNFINISHED later occurrences of its own recurrence chain. Finished later
// occurrences are history and keep their owner, and a clone that shares the
// task's lineage_id — a different job copied from it — is not touched: the
// chain is followed through previous_occurrence_id, not lineage.
func TestSetTaskOwnerFollowsTheChain(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()

	first := seedOccurrence(t, store, oldOwner, uuid.Nil, nil, models.TaskStatusSuccess)
	second := seedOccurrence(t, store, oldOwner, first.LineageID, &first.ID, models.TaskStatusSuccess)
	live := seedOccurrence(t, store, oldOwner, first.LineageID, &second.ID, models.TaskStatusScheduled)
	clone := seedOccurrence(t, store, oldOwner, first.LineageID, nil, models.TaskStatusScheduled)

	updated, err := store.SetTaskOwner(ctx, first.ID, newOwner)
	if err != nil {
		t.Fatalf("SetTaskOwner: %v", err)
	}
	if len(updated) != 2 || updated[0] != first.ID || !slices.Contains(updated, live.ID) {
		t.Fatalf("updated = %v, want [%s %s] (the task first)", updated, first.ID, live.ID)
	}
	for id, want := range map[uuid.UUID]uuid.UUID{first.ID: newOwner, live.ID: newOwner, second.ID: oldOwner, clone.ID: oldOwner} {
		if got := ownerOf(t, store, id); got != want {
			t.Errorf("owner of %s = %s, want %s", id, got, want)
		}
	}

	if _, err := store.SetTaskOwner(ctx, uuid.New(), newOwner); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing task = %v, want sql.ErrNoRows", err)
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
	if _, err := store.SetTaskOwner(ctx, done.ID, newOwner); err != nil {
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
