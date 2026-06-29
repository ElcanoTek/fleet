package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// seedPending inserts a pending task with explicit priority / effective_priority
// / creation time for the priority-queue tests (#230). It constructs the Task
// directly (bypassing NewTask) so the test controls created_at and the effective
// priority precisely.
func seedPending(t *testing.T, db *Database, prio, effective int, createdAt time.Time) *models.Task {
	t.Helper()
	task := &models.Task{
		ID:                uuid.New(),
		Prompt:            "p",
		Status:            models.TaskStatusPending,
		Priority:          prio,
		EffectivePriority: effective,
		CreatedAt:         createdAt,
	}
	if err := db.AddTask(context.Background(), task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task
}

// TestClaimOrdersByEffectivePriority is acceptance criterion #3 (#230): a
// high-urgency task that arrives LATER is still claimed before an already-queued
// low-urgency (bulk) task — urgency beats FIFO across tiers.
func TestClaimOrdersByEffectivePriority(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	// Bulk queued first; critical arrives a minute later.
	bulk := seedPending(t, db, models.PriorityBulk, models.PriorityBulk, now.Add(-time.Minute))
	crit := seedPending(t, db, models.PriorityCritical, models.PriorityCritical, now)

	first, err := db.ClaimNextPendingTask(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if first == nil || first.ID != crit.ID {
		t.Fatalf("expected the critical task claimed first despite arriving later, got %v", first)
	}
	second, err := db.ClaimNextPendingTask(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if second == nil || second.ID != bulk.ID {
		t.Fatalf("expected the bulk task claimed second, got %v", second)
	}
}

// TestClaimFIFOWithinTier: two tasks at the same effective priority are claimed
// oldest-first (#230).
func TestClaimFIFOWithinTier(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	older := seedPending(t, db, models.PriorityNormal, models.PriorityNormal, now.Add(-time.Minute))
	seedPending(t, db, models.PriorityNormal, models.PriorityNormal, now)

	first, err := db.ClaimNextPendingTask(ctx, "w", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first == nil || first.ID != older.ID {
		t.Fatalf("expected the older same-tier task first (FIFO), got %v", first)
	}
}

// TestPromoteStarvedTasks: a bulk task that has waited past the window is
// promoted to the starvation floor (its submitted priority untouched); a fresh
// bulk task and an already-urgent task are left alone; the sweep is idempotent
// and a non-positive window is a no-op (#230).
func TestPromoteStarvedTasks(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	starving := seedPending(t, db, models.PriorityBulk, models.PriorityBulk, now.Add(-2*time.Hour))
	fresh := seedPending(t, db, models.PriorityBulk, models.PriorityBulk, now)
	critical := seedPending(t, db, models.PriorityCritical, models.PriorityCritical, now.Add(-2*time.Hour))

	n, err := db.PromoteStarvedTasks(ctx, 30) // 30-minute window
	if err != nil {
		t.Fatalf("PromoteStarvedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 promotion, got %d", n)
	}

	got, err := db.GetTask(ctx, starving.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.EffectivePriority != models.StarvationFloorPriority {
		t.Errorf("starving effective_priority = %d, want %d (floor)", got.EffectivePriority, models.StarvationFloorPriority)
	}
	if got.Priority != models.PriorityBulk {
		t.Errorf("submitted priority must be untouched, got %d want %d", got.Priority, models.PriorityBulk)
	}

	freshGot, _ := db.GetTask(ctx, fresh.ID)
	if freshGot.EffectivePriority != models.PriorityBulk {
		t.Errorf("fresh task should not be promoted, effective = %d", freshGot.EffectivePriority)
	}
	critGot, _ := db.GetTask(ctx, critical.ID)
	if critGot.EffectivePriority != models.PriorityCritical {
		t.Errorf("already-urgent task should not be touched, effective = %d", critGot.EffectivePriority)
	}

	// Idempotent: the now-promoted task is no longer > floor, so a second sweep
	// promotes nothing new.
	if n2, err := db.PromoteStarvedTasks(ctx, 30); err != nil || n2 != 0 {
		t.Errorf("second sweep should promote nothing, got (%d, %v)", n2, err)
	}
	// A non-positive window disables the sweep entirely.
	if n3, err := db.PromoteStarvedTasks(ctx, 0); err != nil || n3 != 0 {
		t.Errorf("window<=0 must be a no-op, got (%d, %v)", n3, err)
	}
}

// TestUpdateTaskPreservesPromotion guards the ON CONFLICT decision (#230):
// because UpdateTask routes through the AddTask upsert, effective_priority is
// excluded from that upsert — otherwise a routine update carrying a stale
// in-memory copy would silently undo an anti-starvation promotion.
func TestUpdateTaskPreservesPromotion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// A bulk task that has already waited past the window.
	task := seedPending(t, db, models.PriorityBulk, models.PriorityBulk, time.Now().UTC().Add(-2*time.Hour))

	// Promote it (raw UPDATE → effective becomes the floor).
	if n, err := db.PromoteStarvedTasks(ctx, 30); err != nil || n != 1 {
		t.Fatalf("promote: (%d, %v)", n, err)
	}

	// Persist a routine update with the STALE in-memory copy (effective is still
	// Bulk in memory). UpdateTask delegates to the AddTask upsert.
	task.Prompt = "edited prompt"
	if err := db.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.EffectivePriority != models.StarvationFloorPriority {
		t.Errorf("promotion clobbered: effective_priority = %d, want %d", got.EffectivePriority, models.StarvationFloorPriority)
	}
	if got.Prompt != "edited prompt" {
		t.Errorf("routine update did not persist: prompt = %q", got.Prompt)
	}
}

// TestPendingQueueStats rolls pending tasks up per effective priority (#230).
func TestPendingQueueStats(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedPending(t, db, models.PriorityCritical, models.PriorityCritical, now.Add(-10*time.Second))
	seedPending(t, db, models.PriorityNormal, models.PriorityNormal, now.Add(-30*time.Second))
	seedPending(t, db, models.PriorityNormal, models.PriorityNormal, now.Add(-5*time.Second))

	buckets, err := db.PendingQueueStats(ctx)
	if err != nil {
		t.Fatalf("PendingQueueStats: %v", err)
	}
	counts := map[int]int{}
	oldest := map[int]int{}
	for _, b := range buckets {
		counts[b.Priority] = b.Count
		oldest[b.Priority] = b.OldestAgeSeconds
	}
	if counts[models.PriorityCritical] != 1 || counts[models.PriorityNormal] != 2 {
		t.Errorf("bucket counts wrong: %+v", counts)
	}
	// The Normal bucket's oldest wait reflects the 30s-old row, not the 5s one.
	if oldest[models.PriorityNormal] < 20 {
		t.Errorf("Normal oldest age = %ds, want >= ~30s", oldest[models.PriorityNormal])
	}
}
