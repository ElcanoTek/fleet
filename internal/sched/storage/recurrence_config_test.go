package storage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRecurringTaskCarriesFullConfigForward locks in the #565 fix: the next
// occurrence of a recurring task must preserve EVERY definition field, not just
// the handful the old hand-maintained TaskCreate literal happened to list.
// Before the fix, occurrence #2+ silently reset allow_network, carry_context,
// output_schema, sandbox_limits, the delegation / task-creation / event-trigger
// capability bits, persona, and the SLA config to their zero values — so, e.g.,
// a "fetch prices every morning" task with allow_network:true failed silently
// from the second run on because its sandbox was resealed --network=none.
//
// This drives the real scheduleNextRecurrence path (DB persist + reload), so it
// also proves the fields survive the AddTask → scanTask round-trip.
func TestRecurringTaskCarriesFullConfigForward(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	owner := uuid.New()

	maxIter := 42
	expected := 30
	orig := &models.Task{
		ID:            uuid.New(),
		Name:          "daily-config-test",
		Title:         "Daily config test",
		Prompt:        "fetch prices every morning",
		Model:         strPtr("anthropic/claude-sonnet-4-5"),
		FallbackModel: strPtr("openai/gpt-5"),
		MaxIterations: &maxIter,
		Priority:      10,
		Description:   "runbook",
		Persona:       "security-auditor",
		Recurrence:    "@daily",
		Timezone:      "America/New_York",
		Status:        models.TaskStatusPending,
		CreatedAt:     time.Now().UTC(),

		// The capability / posture bits the old literal dropped:
		AllowNetwork:               true,
		CarryContext:               true,
		AllowEventTriggers:         true,
		AllowDelegation:            true,
		AllowTaskCreation:          true,
		AllowRecurringTaskCreation: true,
		InstructionSelfImprove:     true,

		OutputSchema:  json.RawMessage(`{"type":"object"}`),
		SandboxLimits: &models.TaskSandboxLimits{MemoryMB: 512, CPUs: 2, Pids: 128},

		ExpectedDurationMinutes: &expected,
		SLAWarnMultiplier:       1.5,
		SLAFailMultiplier:       3.0,

		Tags:  []string{"nightly"},
		Files: []string{"prices.csv"},
	}
	if _, err := store.AddTask(orig); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Run it to success — this triggers scheduleNextRecurrence.
	assigned, err := store.leaseTaskToOwner(orig.ID, owner)
	if err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	assertMissingStructuredOutputDoesNotRecur(t, store, assigned.ID, owner)
	if _, err := store.UpdateTaskStatusAtomic(assigned.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, Message: strPtr("done"), OutputJSON: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}

	// Find the next occurrence (the row that is not the original).
	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	var next *models.Task
	for _, tk := range all {
		if tk.ID != orig.ID {
			next = tk
			break
		}
	}
	if next == nil {
		t.Fatal("no next recurrence was scheduled")
	}

	// Recurring occurrences are unnamed (the partial unique index is on non-empty
	// names; carrying the name would collide with the completed row).
	if next.Name != "" {
		t.Errorf("next occurrence should be unnamed, got %q", next.Name)
	}
	// The TITLE, by contrast, must be carried: it is a non-unique display label,
	// and a job whose title vanished after its first run would be exactly the
	// "which one is this?" list the title exists to fix.
	if next.Title != "Daily config test" {
		t.Errorf("next occurrence title = %q, want it carried from the completed occurrence", next.Title)
	}

	// Every definition field must survive.
	if !next.AllowNetwork {
		t.Error("allow_network was lost on the recurrence (the headline #565 failure)")
	}
	if !next.CarryContext {
		t.Error("carry_context was lost on the recurrence")
	}
	if !next.AllowEventTriggers {
		t.Error("allow_event_triggers was lost on the recurrence")
	}
	if !next.AllowDelegation {
		t.Error("allow_delegation was lost on the recurrence")
	}
	if !next.AllowTaskCreation {
		t.Error("allow_task_creation was lost on the recurrence")
	}
	if !next.AllowRecurringTaskCreation {
		t.Error("allow_recurring_task_creation was lost on the recurrence")
	}
	if !next.InstructionSelfImprove {
		t.Error("instruction_self_improve was lost on the recurrence")
	}
	if next.Persona != "security-auditor" {
		t.Errorf("persona was lost on the recurrence: got %q", next.Persona)
	}
	if len(next.OutputSchema) == 0 {
		t.Error("output_schema was lost on the recurrence")
	}
	assertNoStructuredOutput(t, next)
	if next.SandboxLimits == nil || next.SandboxLimits.MemoryMB != 512 || next.SandboxLimits.CPUs != 2 || next.SandboxLimits.Pids != 128 {
		t.Errorf("sandbox_limits were lost/altered on the recurrence: %+v", next.SandboxLimits)
	}
	if next.ExpectedDurationMinutes == nil || *next.ExpectedDurationMinutes != 30 {
		t.Errorf("expected_duration_minutes (SLA) was lost on the recurrence: %v", next.ExpectedDurationMinutes)
	}
	if next.SLAWarnMultiplier != 1.5 || next.SLAFailMultiplier != 3.0 {
		t.Errorf("SLA multipliers were lost on the recurrence: warn=%v fail=%v", next.SLAWarnMultiplier, next.SLAFailMultiplier)
	}
	if next.Model == nil || *next.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("model was lost on the recurrence: %v", next.Model)
	}
	if next.FallbackModel == nil || *next.FallbackModel != "openai/gpt-5" {
		t.Errorf("fallback_model was lost on the recurrence: %v", next.FallbackModel)
	}
	if next.MaxIterations == nil || *next.MaxIterations != 42 {
		t.Errorf("max_iterations was lost on the recurrence: %v", next.MaxIterations)
	}
	if next.Timezone != "America/New_York" {
		t.Errorf("timezone was lost on the recurrence: %q", next.Timezone)
	}
	if next.Description != "runbook" {
		t.Errorf("description was lost on the recurrence: %q", next.Description)
	}
}

func assertMissingStructuredOutputDoesNotRecur(t *testing.T, store *Storage, taskID, owner uuid.UUID) {
	t.Helper()
	if _, err := store.UpdateTaskStatusAtomic(taskID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, Message: strPtr("missing output"),
	}); err == nil {
		t.Fatal("structured recurrence succeeded without output_json")
	}
	tasks, err := store.GetAllTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("refused structured success spawned recurrence: got %d tasks", len(tasks))
	}
}

func assertNoStructuredOutput(t *testing.T, task *models.Task) {
	t.Helper()
	if len(task.OutputJSON) != 0 {
		t.Errorf("next occurrence inherited prior output_json: %s", task.OutputJSON)
	}
}
