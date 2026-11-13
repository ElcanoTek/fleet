package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestRecordedModelRolloutUpgradesInFlightSuccessor(t *testing.T) {
	t.Parallel()

	s := New()
	t0 := time.Date(2026, 11, 13, 14, 0, 0, 0, time.UTC)

	running := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Hourly pacing check",
		Recurrence:    "@hourly",
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		CreatedAt:     t0.Add(-10 * time.Minute),
	}

	// Operator rolls out Hydrogen + Opus 5.5 while this occurrence is mid-run.
	newPrimary := "google/gemini-4-hydrogen"
	newFallback := "anthropic/claude-opus-5.5"
	s.rolloutHistory = append(s.rolloutHistory, recordedTaskModelRollout{
		at: t0,
		opts: models.TaskModelRollout{
			Model:         &newPrimary,
			FallbackModel: &newFallback,
		},
	})

	tc := models.TaskCreate{
		Model:         running.Model,
		FallbackModel: running.FallbackModel,
	}
	s.applyRecordedModelRollouts(running, &tc)
	if tc.Model == nil || *tc.Model != "google/gemini-4-hydrogen" {
		t.Fatalf("nextModel = %v, want google/gemini-4-hydrogen", tc.Model)
	}
	if tc.FallbackModel == nil || *tc.FallbackModel != "anthropic/claude-opus-5.5" {
		t.Fatalf("nextFallback = %v, want anthropic/claude-opus-5.5", tc.FallbackModel)
	}
	// Running occurrence's own pointers must not be mutated retroactively.
	if *running.Model != "google/gemini-4-argon" {
		t.Errorf("running task Model mutated to %q", *running.Model)
	}
}

func TestRolloutTaskModels_PostgresAndInFlightRecurrence(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	scheduledAt := time.Now().Add(1 * time.Hour).UTC()
	schedTask := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Daily report",
		Status:        models.TaskStatusScheduled,
		ScheduledFor:  &scheduledAt,
		Recurrence:    "@daily",
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := store.AddTask(schedTask); err != nil {
		t.Fatalf("AddTask(scheduled): %v", err)
	}

	inFlight := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Running cron task",
		Status:        models.TaskStatusPending,
		Recurrence:    "@hourly",
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := store.AddTask(inFlight); err != nil {
		t.Fatalf("AddTask(inFlight): %v", err)
	}
	worker := uuid.New()
	if _, err := store.leaseTaskToOwner(inFlight.ID, worker); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}

	primary := "google/gemini-4-hydrogen"
	fallback := "anthropic/claude-opus-5.5"
	dryRes, err := store.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, true)
	if err != nil {
		t.Fatalf("RolloutTaskModels(dryRun): %v", err)
	}
	if dryRes.UpdatedCount != 1 || dryRes.RecurringUpdatedCount != 1 {
		t.Fatalf("dryRun result = %+v, want Updated=1 Recurring=1", dryRes)
	}

	applyRes, err := store.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, false)
	if err != nil {
		t.Fatalf("RolloutTaskModels(apply): %v", err)
	}
	if applyRes.UpdatedCount != 1 || applyRes.PrimaryUpdatedCount != 1 || applyRes.FallbackUpdatedCount != 1 {
		t.Fatalf("apply result = %+v", applyRes)
	}

	gotSched, err := store.GetTask(schedTask.ID)
	if err != nil {
		t.Fatalf("GetTask(sched): %v", err)
	}
	if gotSched.Model == nil || *gotSched.Model != primary {
		t.Errorf("scheduled task model = %v, want %q", gotSched.Model, primary)
	}
	if gotSched.FallbackModel == nil || *gotSched.FallbackModel != fallback {
		t.Errorf("scheduled task fallback = %v, want %q", gotSched.FallbackModel, fallback)
	}

	// Complete the in-flight leased occurrence: its historical row stays Argon,
	// while its newly spawned scheduled occurrence automatically gets Hydrogen + Opus 5.5.
	if _, err := store.UpdateTaskStatusAtomic(inFlight.ID, worker, &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: strPtr("ok"),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}

	all, err := listAllTasks(store)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	var spawned *models.Task
	for _, tk := range all {
		if tk.ID != schedTask.ID && tk.ID != inFlight.ID {
			spawned = tk
		}
	}
	if spawned == nil {
		t.Fatal("expected spawned recurring occurrence")
	}
	if spawned.Model == nil || *spawned.Model != primary {
		t.Errorf("spawned occurrence model = %v, want %q", spawned.Model, primary)
	}
	if spawned.FallbackModel == nil || *spawned.FallbackModel != fallback {
		t.Errorf("spawned occurrence fallback = %v, want %q", spawned.FallbackModel, fallback)
	}
}
