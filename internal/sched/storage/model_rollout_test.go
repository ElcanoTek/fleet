package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestRecurrenceModelOverrideApplyToPointers(t *testing.T) {
	t.Parallel()

	curM := "google/gemini-4-argon"
	curFB := "anthropic/claude-opus-5"
	newM := "google/gemini-4-hydrogen"
	ov := db.RecurrenceModelOverride{
		HasModel:    true,
		Model:       &newM,
		HasFallback: true,
		// nil FallbackModel with HasFallback=true clears fallback to nil.
		FallbackModel: nil,
	}

	gotM, gotFB := ov.ApplyToPointers(&curM, &curFB)
	if gotM == nil || *gotM != "google/gemini-4-hydrogen" {
		t.Fatalf("gotM = %v, want google/gemini-4-hydrogen", gotM)
	}
	if gotFB != nil {
		t.Fatalf("gotFB = %v, want nil", gotFB)
	}
	if curM != "google/gemini-4-argon" {
		t.Errorf("caller pointer mutated to %q", curM)
	}
}

func TestRolloutTaskModels_DurableAcrossRestartAndPreservesPostRolloutEdits(t *testing.T) {
	t.Parallel()

	store, rawDB := newTestStore(t)
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
		CreatedAt:     time.Now().Add(-5 * time.Minute).UTC(),
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
		CreatedAt:     time.Now().Add(-5 * time.Minute).UTC(),
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
	if dryRes.UpdatedCount != 2 || dryRes.RecurringUpdatedCount != 2 {
		t.Fatalf("dryRun result = %+v, want Updated=2 Recurring=2 (scheduled + in-flight)", dryRes)
	}

	applyRes, err := store.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, false)
	if err != nil {
		t.Fatalf("RolloutTaskModels(apply): %v", err)
	}
	if applyRes.UpdatedCount != 2 || applyRes.PrimaryUpdatedCount != 2 || applyRes.FallbackUpdatedCount != 2 || applyRes.RecurringUpdatedCount != 2 {
		t.Fatalf("apply result = %+v", applyRes)
	}

	// Simulate a full server restart before inFlight finishes: a brand-new Storage
	// instance has zero in-process rollout state and relies solely on PostgreSQL.
	restarted := New()
	restarted.SetDatabase(rawDB)
	restarted.SetTimezone("UTC")

	// 1) Complete the in-flight leased occurrence on the restarted Storage:
	// its own historical row stays Argon, while its successor gets Hydrogen + Opus 5.5.
	if _, err := restarted.UpdateTaskStatusAtomic(inFlight.ID, worker, &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: strPtr("ok"),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic(inFlight): %v", err)
	}

	histInFlight, err := restarted.GetTask(inFlight.ID)
	if err != nil {
		t.Fatalf("GetTask(inFlight): %v", err)
	}
	if histInFlight.Model == nil || *histInFlight.Model != "google/gemini-4-argon" {
		t.Errorf("historical inFlight model = %v, want google/gemini-4-argon", histInFlight.Model)
	}

	// 2) Operator subsequently edits schedTask AFTER the rollout to pick a custom
	// primary & fallback model. When that edited occurrence runs and completes,
	// its successor must preserve the operator's post-rollout edit.
	edit := TaskEditFromTask(schedTask)
	edit.Model = strPtr("openai/gpt-6-luna-pro")
	edit.FallbackModel = strPtr("anthropic/claude-haiku-5.5")
	dueNow := time.Now().Add(-1 * time.Second).UTC()
	edit.ScheduledFor = &dueNow
	if _, err := restarted.UpdateEditableTask(ctx, schedTask.ID, edit); err != nil {
		t.Fatalf("UpdateEditableTask: %v", err)
	}
	if _, err := rawDB.UpdateTasksStatusBatch(ctx, []uuid.UUID{schedTask.ID}, models.TaskStatusScheduled, models.TaskStatusPending); err != nil {
		t.Fatalf("promote scheduled->pending: %v", err)
	}
	worker2 := uuid.New()
	if _, err := restarted.leaseTaskToOwner(schedTask.ID, worker2); err != nil {
		t.Fatalf("leaseTaskToOwner(schedTask): %v", err)
	}
	if _, err := restarted.UpdateTaskStatusAtomic(schedTask.ID, worker2, &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: strPtr("done"),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic(schedTask): %v", err)
	}

	all, err := listAllTasks(restarted)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	var inFlightSuccessor, editedSuccessor *models.Task
	for _, tk := range all {
		if tk.PreviousOccurrenceID == nil {
			continue
		}
		switch *tk.PreviousOccurrenceID {
		case inFlight.ID:
			inFlightSuccessor = tk
		case schedTask.ID:
			editedSuccessor = tk
		}
	}
	if inFlightSuccessor == nil {
		t.Fatal("expected spawned successor for inFlight recurring task")
	}
	if inFlightSuccessor.Model == nil || *inFlightSuccessor.Model != primary {
		t.Errorf("inFlightSuccessor model = %v, want %q", inFlightSuccessor.Model, primary)
	}
	if inFlightSuccessor.FallbackModel == nil || *inFlightSuccessor.FallbackModel != fallback {
		t.Errorf("inFlightSuccessor fallback = %v, want %q", inFlightSuccessor.FallbackModel, fallback)
	}

	if editedSuccessor == nil {
		t.Fatal("expected spawned successor for edited recurring task")
	}
	if editedSuccessor.Model == nil || *editedSuccessor.Model != "openai/gpt-6-luna-pro" {
		t.Errorf("editedSuccessor model = %v, want openai/gpt-6-luna-pro (post-rollout edit must win)", editedSuccessor.Model)
	}
	if editedSuccessor.FallbackModel == nil || *editedSuccessor.FallbackModel != "anthropic/claude-haiku-5.5" {
		t.Errorf("editedSuccessor fallback = %v, want anthropic/claude-haiku-5.5", editedSuccessor.FallbackModel)
	}
}
