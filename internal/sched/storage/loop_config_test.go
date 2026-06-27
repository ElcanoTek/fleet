package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestLoopConfigRoundTrip pins the nullable loop_config JSONB column (#179): nil
// (one-shot) and a populated loop config must round-trip without corruption.
func TestLoopConfigRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// nil → one-shot task → reads back nil.
	oneShot := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, oneShot); err != nil {
		t.Fatalf("add one-shot: %v", err)
	}
	if got, err := store.GetTask(oneShot.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.LoopConfig != nil {
		t.Errorf("nil loop_config must round-trip as nil, got %#v", got.LoopConfig)
	}

	// Populated → reads back equal.
	looped := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(),
		LoopConfig: &models.LoopConfig{
			MaxIterations: 3,
			ExitCondition: "regex:DONE",
			MaxCostUSD:    1.5,
		},
	}
	if _, err := store.AddTaskWithContext(ctx, looped); err != nil {
		t.Fatalf("add looped: %v", err)
	}
	got, err := store.GetTask(looped.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LoopConfig == nil {
		t.Fatal("populated loop_config must round-trip as non-nil")
	}
	if got.LoopConfig.MaxIterations != 3 || got.LoopConfig.ExitCondition != "regex:DONE" || got.LoopConfig.MaxCostUSD != 1.5 {
		t.Errorf("loop_config did not round-trip: %#v", got.LoopConfig)
	}
}

// TestTaskIterationsRoundTrip exercises the task_iterations upsert + list: a row
// created at iteration start is finalized at iteration end, and List returns
// them in iteration order.
func TestTaskIterationsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Iteration 1: created running, then finalized passed (upsert on id).
	it := &models.TaskIteration{
		ID: uuid.New(), TaskID: task.ID, IterationNumber: 1,
		StartedAt: time.Now().UTC(), Status: models.IterationStatusRunning,
	}
	if err := store.AddTaskIteration(ctx, it); err != nil {
		t.Fatalf("add iteration: %v", err)
	}
	done := time.Now().UTC()
	it.CompletedAt = &done
	it.Status = models.IterationStatusPassed
	it.ExitConditionResult = "regex:matched"
	it.CostUSD = 0.25
	it.PromptTokens = 1000
	it.CompletionTokens = 200
	if err := store.AddTaskIteration(ctx, it); err != nil {
		t.Fatalf("finalize iteration: %v", err)
	}

	// A second iteration, recorded out of order to prove ORDER BY.
	it2 := &models.TaskIteration{
		ID: uuid.New(), TaskID: task.ID, IterationNumber: 2,
		StartedAt: time.Now().UTC(), Status: models.IterationStatusFailed,
	}
	if err := store.AddTaskIteration(ctx, it2); err != nil {
		t.Fatalf("add iteration 2: %v", err)
	}

	got, err := store.ListTaskIterations(ctx, task.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 iterations, got %d", len(got))
	}
	if got[0].IterationNumber != 1 || got[1].IterationNumber != 2 {
		t.Errorf("iterations out of order: %d, %d", got[0].IterationNumber, got[1].IterationNumber)
	}
	first := got[0]
	if first.Status != models.IterationStatusPassed || first.ExitConditionResult != "regex:matched" ||
		first.CostUSD != 0.25 || first.PromptTokens != 1000 || first.CompletionTokens != 200 || first.CompletedAt == nil {
		t.Errorf("iteration 1 finalize did not persist: %#v", first)
	}
}
