package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestCleanupOldRuns pins the keep-last-N retention semantics (#252): old
// terminal runs beyond keepPerTask are pruned, while the most-recent keepPerTask
// per bucket, non-terminal tasks, and within-retention runs are all preserved.
func TestCleanupOldRuns(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	mk := func(prompt, recurrence string, status models.TaskStatus, completedDaysAgo int) *models.Task {
		completed := time.Now().UTC().AddDate(0, 0, -completedDaysAgo)
		return &models.Task{
			ID: uuid.New(), Prompt: prompt, Recurrence: recurrence, Status: status,
			CreatedAt: completed, CompletedAt: &completed,
		}
	}

	// One recurring bucket with 12 OLD (>90d) terminal runs at distinct ages.
	old := make([]*models.Task, 0, 12)
	for i := 0; i < 12; i++ {
		tk := mk("recurring job", "0 9 * * *", models.TaskStatusSuccess, 100+i) // 100..111 days ago
		old = append(old, tk)
		if _, err := store.AddTaskWithContext(ctx, tk); err != nil {
			t.Fatalf("add old[%d]: %v", i, err)
		}
	}
	// A long-stale PENDING task — must NEVER be pruned (non-terminal).
	pending := mk("recurring job", "0 9 * * *", models.TaskStatusPending, 200)
	pending.CompletedAt = nil
	if _, err := store.AddTaskWithContext(ctx, pending); err != nil {
		t.Fatalf("add pending: %v", err)
	}
	// A recent terminal run (within retention) — must NOT be pruned.
	recent := mk("recurring job", "0 9 * * *", models.TaskStatusSuccess, 1)
	if _, err := store.AddTaskWithContext(ctx, recent); err != nil {
		t.Fatalf("add recent: %v", err)
	}

	// Retention 90d, keep 10/task: of the 12 old runs, the 10 most-recent are kept
	// → exactly the 2 oldest (111d, 110d) are pruned.
	deleted, err := store.CleanupOldRuns(ctx, 90, 10)
	if err != nil {
		t.Fatalf("CleanupOldRuns: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 pruned (12 old - keep 10), got %d", deleted)
	}

	// The 2 oldest (last two appended: 110d, 111d) are gone; the rest survive.
	for i, tk := range old {
		got, _ := store.GetTask(tk.ID)
		if i >= 10 { // the 2 oldest
			if got != nil {
				t.Errorf("old[%d] (%dd) should have been pruned", i, 100+i)
			}
		} else if got == nil {
			t.Errorf("old[%d] (%dd) should have been kept (within keep-10)", i, 100+i)
		}
	}
	if got, _ := store.GetTask(pending.ID); got == nil {
		t.Error("stale PENDING task must never be pruned")
	}
	if got, _ := store.GetTask(recent.ID); got == nil {
		t.Error("recent (within-retention) run must not be pruned")
	}

	// retentionDays<=0 disables pruning entirely.
	if n, err := store.CleanupOldRuns(ctx, 0, 10); err != nil || n != 0 {
		t.Errorf("retention<=0 must be a no-op, got (%d, %v)", n, err)
	}
}
