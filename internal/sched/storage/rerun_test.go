package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestSourceTaskIDRoundTripAndFilter pins the lineage column (#270): an original
// task round-trips with no source; a re-run carries source_task_id; and the
// ?source_task_id filter lists exactly the re-runs of a given source.
func TestSourceTaskIDRoundTripAndFilter(t *testing.T) {
	store, database := newTestStore(t)
	ctx := context.Background()

	orig := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, orig); err != nil {
		t.Fatalf("add orig: %v", err)
	}
	if got, err := store.GetTask(orig.ID); err != nil {
		t.Fatalf("get orig: %v", err)
	} else if got.SourceTaskID != nil {
		t.Errorf("original task must have nil source_task_id, got %v", got.SourceTaskID)
	}

	srcID := orig.ID
	rerun := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), SourceTaskID: &srcID}
	if _, err := store.AddTaskWithContext(ctx, rerun); err != nil {
		t.Fatalf("add rerun: %v", err)
	}
	got, err := store.GetTask(rerun.ID)
	if err != nil {
		t.Fatalf("get rerun: %v", err)
	}
	if got.SourceTaskID == nil || *got.SourceTaskID != orig.ID {
		t.Errorf("source_task_id did not round-trip, got %v want %v", got.SourceTaskID, orig.ID)
	}

	// Lineage filter returns only the re-run.
	tasks, _, err := database.GetTasksFiltered(ctx, db.TaskFilter{SourceTaskID: &srcID}, 100, 0)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != rerun.ID {
		t.Errorf("source_task_id filter should return exactly the re-run, got %d task(s)", len(tasks))
	}
}
