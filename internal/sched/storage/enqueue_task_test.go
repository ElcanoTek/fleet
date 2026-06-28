package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestEnqueueTask_LineageRoundTrip proves the storage-layer task-create plumbing
// the create_task tool calls (#277) persists the spawn lineage + capability flags
// and reads them back, so audit queries and a re-run keep the right posture.
func TestEnqueueTask_LineageRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	parentID := uuid.New()
	model := "openai/gpt-test"
	id, status, _, err := store.EnqueueTask(ctx, models.TaskCreate{
		Prompt:                     "spawned follow-up",
		Model:                      &model,
		CreatedByTaskID:            &parentID,
		AllowTaskCreation:          true,
		AllowRecurringTaskCreation: true,
	})
	if err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}
	if status != string(models.TaskStatusPending) {
		t.Fatalf("expected pending status for an immediate task, got %q", status)
	}

	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.CreatedByTaskID == nil || *got.CreatedByTaskID != parentID {
		t.Fatalf("expected created_by_task_id %s, got %v", parentID, got.CreatedByTaskID)
	}
	if !got.AllowTaskCreation || !got.AllowRecurringTaskCreation {
		t.Fatalf("expected capability flags to round-trip, got allow=%v recurring=%v",
			got.AllowTaskCreation, got.AllowRecurringTaskCreation)
	}
}

// TestEnqueueTask_RejectsBadCron proves an invalid recurrence is rejected before
// anything is persisted.
func TestEnqueueTask_RejectsBadCron(t *testing.T) {
	store, _ := newTestStore(t)
	if _, _, _, err := store.EnqueueTask(context.Background(), models.TaskCreate{
		Prompt:     "bad cron",
		Recurrence: "not a cron",
	}); err == nil {
		t.Fatal("expected EnqueueTask to reject an invalid cron expression")
	}
}

// TestEnqueueTask_DefaultsNoCapabilityFlags proves the secure default: a task
// created without the flags has both capability bits false (so it cannot
// self-schedule when later run).
func TestEnqueueTask_DefaultsNoCapabilityFlags(t *testing.T) {
	store, _ := newTestStore(t)
	id, _, _, err := store.EnqueueTask(context.Background(), models.TaskCreate{Prompt: "plain task"})
	if err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}
	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.AllowTaskCreation || got.AllowRecurringTaskCreation {
		t.Fatalf("expected capability flags default false, got allow=%v recurring=%v",
			got.AllowTaskCreation, got.AllowRecurringTaskCreation)
	}
}
