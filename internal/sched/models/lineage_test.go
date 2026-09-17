// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package models

import (
	"testing"

	"github.com/google/uuid"
)

// A task created fresh is its own lineage; every copy the clone recipe makes —
// recurrence occurrences, re-runs, clones — joins it, so all runs of one job
// share one working directory (#1543).
func TestLineageDefaultsToOwnIDAndIsCarriedByTheCloneRecipe(t *testing.T) {
	first := NewTask(TaskCreate{Prompt: "daily scan", Recurrence: "0 9 * * *"})
	if first.LineageID == uuid.Nil || first.LineageID != first.ID {
		t.Fatalf("a fresh task must be its own lineage: id=%s lineage=%s", first.ID, first.LineageID)
	}
	if first.WorkspaceLineage() != first.ID {
		t.Fatalf("WorkspaceLineage = %s, want %s", first.WorkspaceLineage(), first.ID)
	}

	second := NewTask(TaskToCreate(first))
	if second.ID == first.ID {
		t.Fatal("a recurrence occurrence is a new row")
	}
	if second.LineageID != first.ID {
		t.Fatalf("occurrence lineage = %s, want the job's %s", second.LineageID, first.ID)
	}
	third := NewTask(TaskToCreate(second))
	if third.LineageID != first.ID {
		t.Fatalf("the lineage must not drift along the chain: %s, want %s", third.LineageID, first.ID)
	}

	// A zero LineageID (a Task assembled in code, or a pre-069 row) reads as
	// the task's own lineage rather than uuid.Nil.
	legacy := &Task{ID: uuid.New()}
	if legacy.WorkspaceLineage() != legacy.ID {
		t.Fatalf("zero lineage must fall back to the task id, got %s", legacy.WorkspaceLineage())
	}
	var none *Task
	if none.WorkspaceLineage() != uuid.Nil {
		t.Fatal("nil task must report uuid.Nil")
	}
	// An explicit nil-UUID pointer is "no lineage", not a lineage of zeros.
	zero := uuid.Nil
	if got := NewTask(TaskCreate{Prompt: "p", LineageID: &zero}); got.LineageID != got.ID {
		t.Fatalf("a nil-UUID lineage pointer must mean fresh, got %s", got.LineageID)
	}
}
