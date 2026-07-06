package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedPendingScheduled seeds a pending task with an explicit created_at AND
// scheduled_for, to exercise the starvation sweep's eligibility clock.
func seedPendingScheduled(t *testing.T, db *Database, createdAt, scheduledFor time.Time) *models.Task {
	t.Helper()
	sf := scheduledFor
	task := &models.Task{
		ID:                uuid.New(),
		Prompt:            "p",
		Status:            models.TaskStatusPending,
		Priority:          models.PriorityBulk,
		EffectivePriority: models.PriorityBulk,
		CreatedAt:         createdAt,
		ScheduledFor:      &sf,
	}
	if err := db.AddTask(context.Background(), task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// TestPromoteStarvedTasks_MeasuresFromEligibility locks in the fix: the sweep
// keys on GREATEST(created_at, scheduled_for), not created_at. A recurring
// occurrence row is created at the PREVIOUS occurrence's completion, so its
// created_at is ~one period old the moment it flips to pending — but its
// scheduled_for is the (near-now) fire time. Keying on created_at would
// floor-promote it instantly (priority inversion); keying on eligibility does
// not. A row whose eligibility time is genuinely old is still promoted.
func TestPromoteStarvedTasks_MeasuresFromEligibility(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	// Recurring-occurrence shape: born long ago, but only just became eligible.
	freshlyEligible := seedPendingScheduled(t, db, now.Add(-24*time.Hour), now.Add(-1*time.Minute))
	// Genuinely starving: eligible well past the window.
	starving := seedPendingScheduled(t, db, now.Add(-24*time.Hour), now.Add(-2*time.Hour))

	n, err := db.PromoteStarvedTasks(ctx, 30) // 30-minute window
	if err != nil {
		t.Fatalf("PromoteStarvedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 promotion (only the genuinely-starving task), got %d", n)
	}

	freshGot, _ := db.GetTask(ctx, freshlyEligible.ID)
	if freshGot.EffectivePriority != models.PriorityBulk {
		t.Errorf("a freshly-eligible (old created_at, recent scheduled_for) task must NOT be promoted; effective = %d", freshGot.EffectivePriority)
	}
	starvedGot, _ := db.GetTask(ctx, starving.ID)
	if starvedGot.EffectivePriority != models.StarvationFloorPriority {
		t.Errorf("a genuinely-starving task must be promoted to the floor; effective = %d", starvedGot.EffectivePriority)
	}
}
