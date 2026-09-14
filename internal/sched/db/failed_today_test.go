package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The dashboard's Failed Today card counted status='error' only, which hid the
// majority of failures: handleRunFailure re-queues a retryable failure and
// dead-letters everything else, so dead_lettered is where a failure normally
// comes to rest and error is the uncommon leftover. A day of quarantined work
// therefore read as a clean day — the counter telling an operator the opposite
// of the truth — and clicking the card, which filtered on error alone, showed
// an empty board under a non-zero count.
//
// Both halves are pinned here: the count, and the filter that has to agree with
// it. They are one test because the defect was the disagreement between them.
func TestFailedTodayCountsBothTerminalFailureStatuses(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM tasks`); err != nil {
		t.Fatalf("clear tasks: %v", err)
	}

	completedNow := time.Now().UTC()
	seed := func(status models.TaskStatus) uuid.UUID {
		t.Helper()
		task := &models.Task{
			ID: uuid.New(), Prompt: "failed-today fixture", Status: status,
			CreatedAt: completedNow, Timezone: "UTC",
		}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
		// AddTask does not stamp completed_at — the runner does, on the terminal
		// transition. DeadLetterTaskWithContext stamps it too, deliberately, "so
		// the row reads as terminal everywhere a completed/errored task does",
		// which is what lets one day-window predicate cover both statuses.
		if _, err := db.conn.ExecContext(ctx,
			`UPDATE tasks SET completed_at = $2 WHERE id = $1`, task.ID, completedNow); err != nil {
			t.Fatalf("stamp completed_at: %v", err)
		}
		return task.ID
	}

	errored := seed(models.TaskStatusError)
	deadLettered := seed(models.TaskStatusDeadLettered)
	seed(models.TaskStatusSuccess)

	stats, err := db.GetDashboardStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.FailedTasksToday != 2 {
		t.Errorf("FailedTasksToday = %d, want 2 (one error + one dead_lettered).\n"+
			"Counting only 'error' is what let a day of quarantined work read as a clean one.",
			stats.FailedTasksToday)
	}
	if stats.CompletedTasksToday != 1 {
		t.Errorf("CompletedTasksToday = %d, want 1 — widening the failure count must not "+
			"change what counts as a success", stats.CompletedTasksToday)
	}

	// The card's own filter. It must return exactly the rows the counter
	// counted, or clicking a non-zero number lands on an empty board.
	tasks, total, err := db.GetTasksFiltered(ctx, TaskFilter{
		CompletedToday: true,
		CompletedStatuses: []string{
			string(models.TaskStatusError),
			string(models.TaskStatusDeadLettered),
		},
	}, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("filtered total = %d, want 2 — the board must show what the counter counted", total)
	}
	got := map[uuid.UUID]bool{}
	for _, task := range tasks {
		got[task.ID] = true
	}
	if !got[errored] || !got[deadLettered] {
		t.Errorf("filtered rows = %v, want both the errored and dead-lettered task", got)
	}

	// The single-value form still works: existing API callers pass one status.
	_, successTotal, err := db.GetTasksFiltered(ctx, TaskFilter{
		CompletedToday:    true,
		CompletedStatuses: []string{string(models.TaskStatusSuccess)},
	}, 50, 0)
	if err != nil {
		t.Fatalf("list success: %v", err)
	}
	if successTotal != 1 {
		t.Errorf("single-status filter total = %d, want 1", successTotal)
	}
}
