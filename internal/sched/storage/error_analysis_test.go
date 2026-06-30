package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestSetTaskErrorAnalysis covers the #317 persistence seam: the diagnosis is
// written only while the task is in a terminal-FAILURE state (the guard), read
// back via GetTask, and a REPLAY clears it so a re-run starts clean.
func TestSetTaskErrorAnalysis(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), MaxRetries: 1}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}
	owner := uuid.New()
	claimed, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, claimed.ID, owner, "boom", 1); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}

	analysis := json.RawMessage(`{"category":"tool_error","summary":"perm denied"}`)
	if err := store.SetTaskErrorAnalysis(ctx, claimed.ID, analysis); err != nil {
		t.Fatalf("set analysis: %v", err)
	}
	got, _ := store.GetTask(claimed.ID)
	if len(got.ErrorAnalysis) == 0 {
		t.Fatal("error_analysis should be set on the dead-lettered task")
	}
	var parsed map[string]any
	if err := json.Unmarshal(got.ErrorAnalysis, &parsed); err != nil || parsed["category"] != "tool_error" {
		t.Fatalf("error_analysis round-trip wrong: %s (%v)", got.ErrorAnalysis, err)
	}

	// Replay (same id → pending) must CLEAR the prior analysis (#317).
	replayed, err := store.ReplayDeadLetteredTask(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(replayed.ErrorAnalysis) != 0 {
		t.Errorf("replayed task (returned) still carries error_analysis: %s", replayed.ErrorAnalysis)
	}
	got, _ = store.GetTask(claimed.ID)
	if len(got.ErrorAnalysis) != 0 {
		t.Errorf("replayed task (persisted) still carries error_analysis: %s", got.ErrorAnalysis)
	}
	if got.Status != models.TaskStatusPending {
		t.Fatalf("replayed status = %s, want pending", got.Status)
	}

	// The terminal-status guard: a late analysis write onto the now-pending
	// (replayed) task is a no-op, so a stale diagnosis can't be stamped onto the
	// fresh attempt.
	if err := store.SetTaskErrorAnalysis(ctx, claimed.ID, json.RawMessage(`{"category":"unknown","summary":"stale"}`)); err != nil {
		t.Fatalf("set analysis (guarded): %v", err)
	}
	got, _ = store.GetTask(claimed.ID)
	if len(got.ErrorAnalysis) != 0 {
		t.Errorf("guard failed: stale analysis was written onto a non-terminal (pending) task: %s", got.ErrorAnalysis)
	}
}
