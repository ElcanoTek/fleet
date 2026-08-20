package db

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The title must survive every write path a task row goes through: the
// single-row INSERT (AddTask), the multi-row one (AddTaskBatch — the path #710
// broke by adding a column without bumping the since-retired manual column
// count), and the UPDATE the edit flow persists through (UpdateTaskTx — the
// path the carry_context regression went missing from). The column lists now
// derive from taskColumnRegistry (#1126), but each write path still gets its
// own end-to-end proof.
func TestTitleRoundTripsThroughEveryWritePath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	t.Run("AddTask", func(t *testing.T) {
		tk := models.NewTask(models.TaskCreate{Prompt: "daily report", Title: "Daily pacing summary"})
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		got, err := db.GetTask(ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Title != "Daily pacing summary" {
			t.Errorf("Title = %q after AddTask, want %q", got.Title, "Daily pacing summary")
		}
	})

	t.Run("AddTaskBatch", func(t *testing.T) {
		a := models.NewTask(models.TaskCreate{Prompt: "batch one", Title: "Batch one"})
		b := models.NewTask(models.TaskCreate{Prompt: "batch two", Title: "Batch two"})
		if err := db.AddTaskBatch(ctx, []*models.Task{a, b}); err != nil {
			t.Fatalf("AddTaskBatch: %v", err)
		}
		for _, want := range []*models.Task{a, b} {
			got, err := db.GetTask(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Title != want.Title {
				t.Errorf("Title = %q after AddTaskBatch, want %q", got.Title, want.Title)
			}
		}
	})

	t.Run("UpdateTaskTx", func(t *testing.T) {
		tk := models.NewTask(models.TaskCreate{Prompt: "retitle me"})
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		got, err := db.GetTask(ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Title != "" {
			t.Fatalf("Title should default to empty (untitled), got %q", got.Title)
		}

		tx, err := db.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		got.Title = "Renamed by an operator"
		if err := db.UpdateTaskTx(ctx, tx, got); err != nil {
			_ = tx.Rollback()
			t.Fatalf("UpdateTaskTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		reread, err := db.GetTask(ctx, tk.ID)
		if err != nil {
			t.Fatalf("GetTask after UpdateTaskTx: %v", err)
		}
		if reread.Title != "Renamed by an operator" {
			t.Errorf("Title = %q after UpdateTaskTx, want the edited value — the edit path dropped it", reread.Title)
		}
	})
}

// Titles are labels, not identities: unlike name (partial UNIQUE index) any
// number of tasks may share one. This is exactly why the display label could not
// simply reuse the name column.
func TestTitlesNeedNotBeUnique(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const shared = "Daily deal health scan"
	for i := range 3 {
		tk := models.NewTask(models.TaskCreate{Prompt: "run the scan", Title: shared})
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask #%d with a duplicate title: %v", i, err)
		}
	}
}
