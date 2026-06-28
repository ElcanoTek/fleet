package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskDescriptionRoundTrip pins the nullable description column (#281): empty
// round-trips as empty, a populated value round-trips intact, and an edit can
// both set and clear it.
func TestTaskDescriptionRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Empty → reads back empty (stored as SQL NULL).
	plain := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, plain); err != nil {
		t.Fatalf("add plain: %v", err)
	}
	if got, err := store.GetTask(plain.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Description != "" {
		t.Errorf("empty description must round-trip empty, got %q", got.Description)
	}

	// Populated → reads back intact (incl. Markdown + multibyte).
	doc := "# Runbook\n\nOwns: platform-team. Cost ≈ $0.50/run. On fail: page #oncall. 日本語 ✅"
	documented := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), Description: doc}
	if _, err := store.AddTaskWithContext(ctx, documented); err != nil {
		t.Fatalf("add documented: %v", err)
	}
	if got, err := store.GetTask(documented.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Description != doc {
		t.Errorf("description did not round-trip:\n got %q\nwant %q", got.Description, doc)
	}

	// UpdateTaskDescription sets then clears.
	if upd, err := store.UpdateTaskDescription(ctx, plain.ID, "now documented"); err != nil {
		t.Fatalf("set description: %v", err)
	} else if upd.Description != "now documented" {
		t.Errorf("set description = %q", upd.Description)
	}
	if upd, err := store.UpdateTaskDescription(ctx, plain.ID, ""); err != nil {
		t.Fatalf("clear description: %v", err)
	} else if upd.Description != "" {
		t.Errorf("clear description left %q", upd.Description)
	}
}

// TestTaskDescriptionFilter pins the has_description filter: only tasks with a
// non-empty description match.
func TestTaskDescriptionFilter(t *testing.T) {
	store, database := newTestStore(t)
	ctx := context.Background()

	documented := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), Description: "has docs"}
	plain := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	for _, tk := range []*models.Task{documented, plain} {
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	tasks, _, err := database.GetTasksFiltered(ctx, db.TaskFilter{HasDescription: true}, 100, 0)
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	ids := map[uuid.UUID]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[documented.ID] {
		t.Error("has_description filter should include the documented task")
	}
	if ids[plain.ID] {
		t.Error("has_description filter must exclude the task with no description")
	}
}
