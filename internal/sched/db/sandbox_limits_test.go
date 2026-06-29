package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestSandboxLimits_RoundTrip persists a task with per-task sandbox limits and
// reads them back unchanged through the JSONB column (#205); a task with no
// override round-trips as nil.
func TestSandboxLimits_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	withLimits := &models.Task{
		ID:            uuid.New(),
		Prompt:        "heavy",
		Status:        models.TaskStatusPending,
		CreatedAt:     time.Now().UTC(),
		SandboxLimits: &models.TaskSandboxLimits{MemoryMB: 2048, CPUs: 2.0, Pids: 512},
	}
	none := &models.Task{
		ID:        uuid.New(),
		Prompt:    "light",
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	for _, tk := range []*models.Task{withLimits, none} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	got, err := db.GetTask(ctx, withLimits.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.SandboxLimits == nil || *got.SandboxLimits != *withLimits.SandboxLimits {
		t.Errorf("round-trip SandboxLimits = %+v, want %+v", got.SandboxLimits, withLimits.SandboxLimits)
	}

	gotNone, err := db.GetTask(ctx, none.ID)
	if err != nil {
		t.Fatalf("GetTask(none): %v", err)
	}
	if gotNone.SandboxLimits != nil {
		t.Errorf("task without limits round-tripped non-nil: %+v", gotNone.SandboxLimits)
	}

	// An UpdateTask (which delegates to the AddTask upsert) preserves the limits.
	got.Prompt = "edited"
	if err := db.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	reGot, _ := db.GetTask(ctx, withLimits.ID)
	if reGot.SandboxLimits == nil || *reGot.SandboxLimits != *withLimits.SandboxLimits {
		t.Errorf("after update, SandboxLimits = %+v, want preserved %+v", reGot.SandboxLimits, withLimits.SandboxLimits)
	}
}
