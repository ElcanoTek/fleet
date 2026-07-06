package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestEnqueueTask_AppliesDefaultTimezone proves the fix for the recurrence
// timezone drift: a recurring task enqueued via the storage seam (the create_task
// tool / chat schedule_task approval / promote-to-task) with no explicit timezone
// must (1) persist the org default-task timezone so scheduleNextRecurrence
// evaluates occurrence #2+ in it, and (2) evaluate the FIRST fire in that zone.
// Before the fix, Timezone persisted as "UTC" and the first fire used the server
// clock zone, so a "9am" recurring task drifted to 9am UTC after occurrence #1.
func TestEnqueueTask_AppliesDefaultTimezone(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetDefaultTaskTimezone("America/New_York")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}

	id, _, _, err := store.EnqueueTask(context.Background(), models.TaskCreate{
		Prompt:     "daily 9am report",
		Recurrence: "0 9 * * *",
	})
	if err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}
	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Timezone != "America/New_York" {
		t.Fatalf("expected persisted timezone America/New_York (so recurrences stay local), got %q", got.Timezone)
	}
	if got.ScheduledFor == nil {
		t.Fatal("expected a scheduled_for for a recurring task with no explicit run")
	}
	// The stored instant is UTC; rendered back in the task's zone it must be 9am.
	if h := got.ScheduledFor.In(ny).Hour(); h != 9 {
		t.Fatalf("first fire should be 09:00 America/New_York, got hour %d (%s)", h, got.ScheduledFor.In(ny))
	}
}

// TestEnqueueTask_ExplicitTimezoneWins proves an explicit tc.Timezone is honored
// over the org default and persisted for the recurrence path.
func TestEnqueueTask_ExplicitTimezoneWins(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetDefaultTaskTimezone("America/New_York")

	id, _, _, err := store.EnqueueTask(context.Background(), models.TaskCreate{
		Prompt:     "tokyo morning",
		Recurrence: "0 9 * * *",
		Timezone:   "Asia/Tokyo",
	})
	if err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}
	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Timezone != "Asia/Tokyo" {
		t.Fatalf("explicit timezone should win, got %q", got.Timezone)
	}
}

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
