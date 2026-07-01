package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestArtifacts_RoundTrip exercises the #204 artifacts JSONB column threading: a
// fresh task has no manifest; the AddTask upsert (UpdateTask) deliberately does
// NOT write artifacts (clobber-safe, like effective_priority); and UpdateTaskTx
// (the runner's atomic status-update seam) writes it and reads back unchanged.
func TestArtifacts_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	tk := &models.Task{
		ID:        uuid.New(),
		Prompt:    "produces a report",
		Status:    models.TaskStatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.AddTask(ctx, tk); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	got, err := db.GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Artifacts != nil {
		t.Errorf("Artifacts should be nil before publish, got %s", got.Artifacts)
	}

	// The AddTask upsert path (UpdateTask delegates to AddTask) must NOT persist
	// artifacts — it is excluded from the ON CONFLICT set, so a stale in-memory
	// value cannot clobber a recorded manifest.
	got.Artifacts = json.RawMessage(`[{"name":"x","path":"x","size":1}]`)
	if err := db.UpdateTask(ctx, got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if reread, _ := db.GetTask(ctx, tk.ID); reread.Artifacts != nil {
		t.Errorf("UpdateTask (AddTask upsert) must NOT write artifacts; got %s", reread.Artifacts)
	}

	// The runner's path (UpdateTaskTx, used by UpdateTaskStatusAtomicWithContext)
	// writes and round-trips the manifest.
	manifest := json.RawMessage(`[{"name":"report.csv","path":"report.csv","description":"Q3","size":12},{"name":"summary.txt","path":"out/summary.txt","size":5}]`)
	tx, err := db.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	got.Artifacts = manifest
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
	if !jsonEqual(reread.Artifacts, manifest) {
		t.Errorf("Artifacts round-trip = %s, want %s", reread.Artifacts, manifest)
	}
}
