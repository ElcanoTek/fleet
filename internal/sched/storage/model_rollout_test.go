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

	// OnlySetFallbacks preserves nil fallbacks on bare API tasks while updating
	// tasks that already have a fallback_model configured.
	fbTarget := "anthropic/claude-opus-5.5"
	r := models.TaskModelRollout{FallbackModel: &fbTarget, OnlySetFallbacks: true}
	if _, nextNilFB, _, fbChanged := r.ApplyToPointers(&curM, nil); nextNilFB != nil || fbChanged {
		t.Errorf("OnlySetFallbacks mutated nil fallback: next=%v changed=%v", nextNilFB, fbChanged)
	}
	if _, nextSetFB, _, fbChanged := r.ApplyToPointers(&curM, &curFB); nextSetFB == nil || *nextSetFB != fbTarget || !fbChanged {
		t.Errorf("OnlySetFallbacks did not update set fallback: next=%v changed=%v", nextSetFB, fbChanged)
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

func TestRolloutTaskModels_PreservesRecurrenceOverrideAcrossUnrelatedEdits(t *testing.T) {
	t.Parallel()

	st, rawDB := newTestStore(t)
	st.SetTimezone("UTC")
	ctx := context.Background()

	// Two recurring tasks that are leased during the rollout.
	tagEditedTask := &models.Task{
		ID:            uuid.New(),
		Prompt:        "recurring tag edit check",
		Status:        models.TaskStatusPending,
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "0 8 * * *",
		CreatedAt:     time.Now().Add(-5 * time.Minute).UTC(),
	}
	if _, err := st.AddTask(tagEditedTask); err != nil {
		t.Fatalf("AddTask(tagEditedTask): %v", err)
	}
	partialModelTask := &models.Task{
		ID:            uuid.New(),
		Prompt:        "recurring partial model edit check",
		Status:        models.TaskStatusPending,
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "0 9 * * *",
		CreatedAt:     time.Now().Add(-5 * time.Minute).UTC(),
	}
	if _, err := st.AddTask(partialModelTask); err != nil {
		t.Fatalf("AddTask(partialModelTask): %v", err)
	}

	deadWorker := uuid.New()
	if _, err := st.leaseTaskToOwner(tagEditedTask.ID, deadWorker); err != nil {
		t.Fatalf("lease tagEditedTask: %v", err)
	}
	if _, err := st.leaseTaskToOwner(partialModelTask.ID, deadWorker); err != nil {
		t.Fatalf("lease partialModelTask: %v", err)
	}

	primary := "google/gemini-4-hydrogen"
	fallback := "anthropic/claude-opus-5.5"
	if _, err := st.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, false); err != nil {
		t.Fatalf("RolloutTaskModels: %v", err)
	}

	// Simulate recovery/resume back to pending while the row still carries the pre-rollout models.
	if _, err := rawDB.UpdateTasksStatusBatch(ctx, []uuid.UUID{tagEditedTask.ID, partialModelTask.ID}, models.TaskStatusLeased, models.TaskStatusPending); err != nil {
		t.Fatalf("recover leased->pending: %v", err)
	}

	// 1) Unrelated tag + description edits on tagEditedTask must NOT drop its rollout override.
	if _, err := st.UpdateTaskTags(ctx, tagEditedTask.ID, []string{"ops"}, nil); err != nil {
		t.Fatalf("UpdateTaskTags: %v", err)
	}
	if _, err := st.UpdateTaskDescription(ctx, tagEditedTask.ID, "operator runbook note"); err != nil {
		t.Fatalf("UpdateTaskDescription: %v", err)
	}

	// 2) Editing ONLY the primary model on partialModelTask clears HasModel while keeping HasFallback.
	curPartial, err := st.GetTask(partialModelTask.ID)
	if err != nil {
		t.Fatalf("GetTask(partialModelTask): %v", err)
	}
	edit := TaskEditFromTask(curPartial)
	edit.Model = strPtr("openai/gpt-6-luna-pro")
	if _, err := st.UpdateEditableTask(ctx, partialModelTask.ID, edit); err != nil {
		t.Fatalf("UpdateEditableTask(partialModelTask): %v", err)
	}

	// Run and complete both occurrences.
	for _, id := range []uuid.UUID{tagEditedTask.ID, partialModelTask.ID} {
		w := uuid.New()
		if _, err := st.leaseTaskToOwner(id, w); err != nil {
			t.Fatalf("lease %s: %v", id, err)
		}
		if _, err := st.UpdateTaskStatusAtomic(id, w, &models.StatusUpdate{
			Status:  models.TaskStatusSuccess,
			Message: strPtr("ok"),
		}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}

	all, err := listAllTasks(st)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	var tagSucc, partialSucc *models.Task
	for _, tk := range all {
		if tk.PreviousOccurrenceID == nil {
			continue
		}
		switch *tk.PreviousOccurrenceID {
		case tagEditedTask.ID:
			tagSucc = tk
		case partialModelTask.ID:
			partialSucc = tk
		}
	}
	if tagSucc == nil {
		t.Fatal("expected successor for tagEditedTask")
	}
	if tagSucc.Model == nil || *tagSucc.Model != primary {
		t.Errorf("tagSucc model = %v, want %q", tagSucc.Model, primary)
	}
	if tagSucc.FallbackModel == nil || *tagSucc.FallbackModel != fallback {
		t.Errorf("tagSucc fallback = %v, want %q", tagSucc.FallbackModel, fallback)
	}
	if partialSucc == nil {
		t.Fatal("expected successor for partialModelTask")
	}
	if partialSucc.Model == nil || *partialSucc.Model != "openai/gpt-6-luna-pro" {
		t.Errorf("partialSucc model = %v, want openai/gpt-6-luna-pro", partialSucc.Model)
	}
	if partialSucc.FallbackModel == nil || *partialSucc.FallbackModel != fallback {
		t.Errorf("partialSucc fallback = %v, want %q", partialSucc.FallbackModel, fallback)
	}
}

func TestRolloutTaskModels_PreservesOverrideAcrossParkedDeadLetterAndSelectiveBatchUpdate(t *testing.T) {
	t.Parallel()

	st, rawDB := newTestStore(t)
	st.SetTimezone("UTC")
	ctx := context.Background()

	// 1) A recurring task leased during rollout that dead-letters into a parked
	// state (malformed execution requirements) must retain its rollout override
	// when replayed with a corrected prompt.
	parkedTask := &models.Task{
		ID:            uuid.New(),
		Prompt:        malformedParkPrompt,
		Status:        models.TaskStatusPending,
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "@daily",
		CreatedAt:     time.Now().Add(-5 * time.Minute).UTC(),
	}
	if _, err := st.AddTask(parkedTask); err != nil {
		t.Fatalf("AddTask(parkedTask): %v", err)
	}
	w1 := uuid.New()
	if _, err := st.leaseTaskToOwner(parkedTask.ID, w1); err != nil {
		t.Fatalf("lease parkedTask: %v", err)
	}

	// 2) Two scheduled tasks leased during rollout, then recovered to scheduled;
	// UpdateScheduledTaskModels(--from-model other, fallbackModel=nil) must leave
	// non-matching overrides untouched and keep fallback overrides on matching rows.
	schedKeep := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Keep override",
		Status:        models.TaskStatusPending,
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "0 6 * * *",
		CreatedAt:     time.Now().Add(-4 * time.Minute).UTC(),
	}
	schedMatch := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Match batch primary only",
		Status:        models.TaskStatusPending,
		Model:         strPtr("openai/gpt-4o"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "0 7 * * *",
		CreatedAt:     time.Now().Add(-3 * time.Minute).UTC(),
	}
	for _, tk := range []*models.Task{schedKeep, schedMatch} {
		if _, err := st.AddTask(tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		if _, err := st.leaseTaskToOwner(tk.ID, uuid.New()); err != nil {
			t.Fatalf("lease %s: %v", tk.ID, err)
		}
	}

	primary := "google/gemini-4-hydrogen"
	fallback := "anthropic/claude-opus-5.5"
	if _, err := st.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, false); err != nil {
		t.Fatalf("RolloutTaskModels: %v", err)
	}

	// Dead-letter parkedTask -> parks chain (recurrence_parked_at set).
	if _, err := st.DeadLetterTaskWithContext(ctx, parkedTask.ID, w1, "non-retryable failure (terminal): execution requirements: …", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext(parkedTask): %v", err)
	}
	afterPark, err := st.GetTask(parkedTask.ID)
	if err != nil || afterPark.RecurrenceParkedAt == nil {
		t.Fatalf("expected parkedTask to be parked, got %+v (err=%v)", afterPark, err)
	}

	// Replay parkedTask with corrected prompt, run to success -> successor must inherit Hydrogen + Opus 5.5.
	if _, err := st.ReplayDeadLetteredTaskWithPrompt(ctx, parkedTask.ID, "Daily sync fixed"); err != nil {
		t.Fatalf("ReplayDeadLetteredTaskWithPrompt: %v", err)
	}
	wReplay := uuid.New()
	if _, err := st.leaseTaskToOwner(parkedTask.ID, wReplay); err != nil {
		t.Fatalf("lease replayed parkedTask: %v", err)
	}
	if _, err := st.UpdateTaskStatusAtomic(parkedTask.ID, wReplay, &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: strPtr("recovered"),
	}); err != nil {
		t.Fatalf("complete replayed parkedTask: %v", err)
	}

	// Move schedKeep and schedMatch to scheduled, then run selective BulkUpdateScheduledTaskModel
	// targeting only openai/gpt-4o with fallbackModel=nil.
	if _, err := rawDB.UpdateTasksStatusBatch(ctx, []uuid.UUID{schedKeep.ID, schedMatch.ID}, models.TaskStatusLeased, models.TaskStatusScheduled); err != nil {
		t.Fatalf("leased->scheduled: %v", err)
	}
	updated, err := st.BulkUpdateScheduledTaskModel(ctx, "openai/gpt-6-luna-pro", nil, "openai/gpt-4o")
	if err != nil || updated != 1 {
		t.Fatalf("BulkUpdateScheduledTaskModel = (%d, %v), want (1, nil)", updated, err)
	}
	if _, err := rawDB.UpdateTasksStatusBatch(ctx, []uuid.UUID{schedKeep.ID, schedMatch.ID}, models.TaskStatusScheduled, models.TaskStatusPending); err != nil {
		t.Fatalf("scheduled->pending: %v", err)
	}
	for _, id := range []uuid.UUID{schedKeep.ID, schedMatch.ID} {
		wk := uuid.New()
		if _, err := st.leaseTaskToOwner(id, wk); err != nil {
			t.Fatalf("lease %s: %v", id, err)
		}
		if _, err := st.UpdateTaskStatusAtomic(id, wk, &models.StatusUpdate{
			Status:  models.TaskStatusSuccess,
			Message: strPtr("done"),
		}); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}

	assertSuccessorModels(t, st, parkedTask.ID, primary, fallback)
	assertSuccessorModels(t, st, schedKeep.ID, primary, fallback)
	assertSuccessorModels(t, st, schedMatch.ID, "openai/gpt-6-luna-pro", fallback)
}

func assertSuccessorModels(t *testing.T, st *Storage, prevID uuid.UUID, wantModel, wantFallback string) {
	t.Helper()
	all, err := listAllTasks(st)
	if err != nil {
		t.Fatalf("listAllTasks: %v", err)
	}
	for _, tk := range all {
		if tk.PreviousOccurrenceID != nil && *tk.PreviousOccurrenceID == prevID {
			if tk.Model == nil || *tk.Model != wantModel || tk.FallbackModel == nil || *tk.FallbackModel != wantFallback {
				t.Errorf("successor of %s = %+v, want %s / %s", prevID, tk, wantModel, wantFallback)
			}
			return
		}
	}
	t.Errorf("no successor spawned for %s", prevID)
}

func TestRolloutTaskModels_ImportOverwriteClearsChangedOverrideFields(t *testing.T) {
	t.Parallel()

	st, rawDB := newTestStore(t)
	st.SetTimezone("UTC")
	ctx := context.Background()

	tk := &models.Task{
		ID:            uuid.New(),
		Prompt:        "Import overwrite test",
		Status:        models.TaskStatusPending,
		Model:         strPtr("google/gemini-4-argon"),
		FallbackModel: strPtr("anthropic/claude-opus-5"),
		Recurrence:    "0 8 * * *",
		CreatedAt:     time.Now().Add(-2 * time.Minute).UTC(),
	}
	if _, err := st.AddTask(tk); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := st.leaseTaskToOwner(tk.ID, uuid.New()); err != nil {
		t.Fatalf("lease: %v", err)
	}

	primary := "google/gemini-4-hydrogen"
	fallback := "anthropic/claude-opus-5.5"
	if _, err := st.RolloutTaskModels(ctx, models.TaskModelRollout{
		Model:         &primary,
		FallbackModel: &fallback,
	}, false); err != nil {
		t.Fatalf("RolloutTaskModels: %v", err)
	}

	// Recover to pending, then import-overwrite (AddTaskWithContext -> AddTaskTx)
	// changing primary model while keeping fallback_model unchanged.
	if _, err := rawDB.UpdateTasksStatusBatch(ctx, []uuid.UUID{tk.ID}, models.TaskStatusLeased, models.TaskStatusPending); err != nil {
		t.Fatalf("leased->pending: %v", err)
	}
	imported := *tk
	imported.Status = models.TaskStatusPending
	imported.Model = strPtr("openai/gpt-6-luna-pro")
	if _, err := st.AddTaskWithContext(ctx, &imported); err != nil {
		t.Fatalf("AddTaskWithContext: %v", err)
	}

	wk := uuid.New()
	if _, err := st.leaseTaskToOwner(tk.ID, wk); err != nil {
		t.Fatalf("lease imported: %v", err)
	}
	if _, err := st.UpdateTaskStatusAtomic(tk.ID, wk, &models.StatusUpdate{
		Status:  models.TaskStatusSuccess,
		Message: strPtr("done"),
	}); err != nil {
		t.Fatalf("complete imported: %v", err)
	}

	assertSuccessorModels(t, st, tk.ID, "openai/gpt-6-luna-pro", fallback)
}
