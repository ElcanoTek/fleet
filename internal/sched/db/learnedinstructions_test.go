package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// #516: feedback capture → distilled proposal (evidence marked consumed) →
// activation/revert with at-most-one-active, all lease-free and versioned.
func TestLearnedInstructionsLifecycle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "weekly report", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	now := time.Now().Unix()

	// Record three down signals + one up.
	var downIDs []uuid.UUID
	for i := 0; i < 3; i++ {
		f := &models.TaskFeedback{ID: uuid.New(), TaskID: task.ID, Rating: models.FeedbackDown, Critique: "too verbose", CreatedAt: now}
		if err := db.AddTaskFeedback(ctx, f); err != nil {
			t.Fatal(err)
		}
		downIDs = append(downIDs, f.ID)
	}
	if err := db.AddTaskFeedback(ctx, &models.TaskFeedback{ID: uuid.New(), TaskID: task.ID, Rating: models.FeedbackUp, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	fresh, err := db.UnconsumedFeedback(ctx, task.ID)
	if err != nil || len(fresh) != 4 {
		t.Fatalf("unconsumed: %d %v", len(fresh), err)
	}

	// Distill v1 from the three down signals → they become consumed.
	li, err := db.ProposeLearnedInstruction(ctx, task.ID, "Keep the summary under 200 words.", downIDs, now)
	if err != nil {
		t.Fatal(err)
	}
	if li.Version != 1 || li.Status != models.LearnedProposed || li.SignalCount != 3 {
		t.Fatalf("proposal: %+v", li)
	}
	after, _ := db.UnconsumedFeedback(ctx, task.ID)
	if len(after) != 1 { // only the up-vote remains fresh
		t.Fatalf("evidence must be consumed: %d remain", len(after))
	}

	// A proposal does NOT change behavior: no active instruction yet.
	if active, _ := db.ActiveLearnedInstruction(ctx, task.ID); active != nil {
		t.Fatalf("proposal must not auto-activate: %+v", active)
	}

	// Activate v1.
	act, err := db.ActivateLearnedInstruction(ctx, task.ID, 1, "alice", now)
	if err != nil || act.Status != models.LearnedActive || act.ActivatedBy != "alice" {
		t.Fatalf("activate: %+v %v", act, err)
	}
	got, _ := db.ActiveLearnedInstruction(ctx, task.ID)
	if got == nil || got.Version != 1 {
		t.Fatalf("active lookup: %+v", got)
	}

	// A second proposal (v2) then activation supersedes v1 (at-most-one-active).
	li2, err := db.ProposeLearnedInstruction(ctx, task.ID, "Also lead with the top-line number.", nil, now)
	if err != nil || li2.Version != 2 {
		t.Fatalf("v2: %+v %v", li2, err)
	}
	if _, err := db.ActivateLearnedInstruction(ctx, task.ID, 2, "bob", now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ActiveLearnedInstruction(ctx, task.ID)
	if got.Version != 2 {
		t.Fatalf("v2 should be active, got v%d", got.Version)
	}
	all, _ := db.ListLearnedInstructions(ctx, task.ID)
	if len(all) != 2 || all[0].Version != 2 {
		t.Fatalf("list newest-first: %+v", all)
	}
	activeCount := 0
	for _, x := range all {
		if x.Status == models.LearnedActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("exactly one active, got %d", activeCount)
	}

	// Revert = re-activate v1.
	if _, err := db.ActivateLearnedInstruction(ctx, task.ID, 1, "alice", now); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ActiveLearnedInstruction(ctx, task.ID)
	if got.Version != 1 {
		t.Fatalf("revert to v1 failed: v%d", got.Version)
	}

	// Full deactivate.
	had, err := db.DeactivateLearnedInstructions(ctx, task.ID)
	if err != nil || !had {
		t.Fatalf("deactivate: %v had=%v", err, had)
	}
	if active, _ := db.ActiveLearnedInstruction(ctx, task.ID); active != nil {
		t.Fatalf("must be none active after deactivate: %+v", active)
	}

	// Activating a missing version errors.
	if _, err := db.ActivateLearnedInstruction(ctx, task.ID, 99, "x", now); err == nil {
		t.Fatal("activating a missing version must error")
	}
}
