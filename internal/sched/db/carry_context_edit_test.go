package db

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A carry_context edit must survive the UpdateTaskTx path — the one the
// PUT /tasks/{id} edit flow persists through (storage.UpdateEditableTask).
// Regression: the column was missing from UpdateTaskTx's SET list, so the
// toggle was acknowledged with 200 (the handler returns the mutated in-memory
// task) and then silently reverted on the next read.
func TestUpdateTaskTxPersistsCarryContext(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	tk := models.NewTask(models.TaskCreate{Prompt: "recurring report", Recurrence: "0 9 * * *"})
	if err := db.AddTask(ctx, tk); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	got, err := db.GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.CarryContext {
		t.Fatal("CarryContext should default to false")
	}

	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	got.CarryContext = true
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
	if !reread.CarryContext {
		t.Error("CarryContext = false after UpdateTaskTx, want true — the edit path dropped the toggle")
	}
}
