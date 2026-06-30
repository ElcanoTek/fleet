package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRecurringTaskCarriesMemoryForward locks in the #285 fix: each cron
// recurrence is a NEW task row (new task_id), and task memory is keyed by
// task_id — so without an explicit carry-forward a recurring "Captain's Log"
// task would start cold every occurrence, defeating the feature. The
// scheduleNextRecurrence path must (a) carry instruction_self_improve forward so
// the next occurrence still gets remember/recall, and (b) copy the completing
// occurrence's memories into the new occurrence's task_id.
func TestRecurringTaskCarriesMemoryForward(t *testing.T) {
	store, database := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	mem := sched.NewStore(database)

	owner := uuid.New()

	orig := &models.Task{ID: uuid.New(), Prompt: "weekly price check", Status: models.TaskStatusPending, Priority: 10, Recurrence: "@daily", InstructionSelfImprove: true, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(orig); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// The first occurrence saves a fact.
	if err := mem.UpsertTaskMemory(ctx, orig.ID, "last_price", "42.17", 100, 4096); err != nil {
		t.Fatalf("UpsertTaskMemory: %v", err)
	}

	// Run it to success — this triggers scheduleNextRecurrence.
	assigned, err := store.leaseTaskToOwner(orig.ID, owner)
	if err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(assigned.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}

	// Find the next occurrence.
	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	var next *models.Task
	for _, tk := range all {
		if tk.ID != orig.ID {
			next = tk
			break
		}
	}
	if next == nil {
		t.Fatal("next recurring occurrence not created")
	}

	// (a) The opt-in flag is carried forward.
	if !next.InstructionSelfImprove {
		t.Error("next occurrence must keep instruction_self_improve so it still gets remember/recall")
	}
	// (b) The fact saved by the prior occurrence is visible to the new one.
	got, err := mem.GetTaskMemory(ctx, next.ID, "last_price")
	if err != nil {
		t.Fatalf("memory not carried forward to the next occurrence: %v", err)
	}
	if got != "42.17" {
		t.Errorf("carried-forward value = %q, want 42.17", got)
	}
}
