package handlers

import (
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestBuildRerunTaskCreate covers the scheduling logic — including the regression
// that a rerun's immediate run must use ScheduledFor=nil (NOT &now), so it
// passes validateTaskCreate's "not in the past" check.
func TestBuildRerunTaskCreate(t *testing.T) {
	h := newValidateTestHandlers()
	src := &models.Task{
		Prompt:     "do the work for the team",
		Priority:   3,
		Recurrence: "0 9 * * *",
		Timezone:   "UTC",
		Tags:       []string{"nightly"},
	}

	t.Run("rerun is immediate, one-time, and validates", func(t *testing.T) {
		tc, err := buildRerunTaskCreate(src, false, taskRerunOverrides{}, time.UTC)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if tc.ScheduledFor != nil {
			t.Errorf("rerun must use nil ScheduledFor (run-now), got %v", tc.ScheduledFor)
		}
		if tc.Recurrence != "" {
			t.Errorf("rerun must clear recurrence, got %q", tc.Recurrence)
		}
		// The regression guard: this must NOT be rejected as "in the past".
		if verr := h.validateTaskCreate(&tc); verr != nil {
			t.Fatalf("rerun recipe must pass validation, got %v", verr)
		}
	})

	t.Run("recurring clone keeps recurrence and schedules a future fire", func(t *testing.T) {
		tc, err := buildRerunTaskCreate(src, true, taskRerunOverrides{}, time.UTC)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if tc.Recurrence != "0 9 * * *" {
			t.Errorf("clone must preserve recurrence, got %q", tc.Recurrence)
		}
		if tc.ScheduledFor == nil || !tc.ScheduledFor.After(time.Now()) {
			t.Errorf("recurring clone must schedule a future fire, got %v", tc.ScheduledFor)
		}
		if verr := h.validateTaskCreate(&tc); verr != nil {
			t.Fatalf("clone recipe must pass validation, got %v", verr)
		}
	})

	t.Run("non-recurring clone is immediate", func(t *testing.T) {
		noRecur := &models.Task{Prompt: "do the work for the team", Timezone: "UTC"}
		tc, err := buildRerunTaskCreate(noRecur, true, taskRerunOverrides{}, time.UTC)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if tc.ScheduledFor != nil {
			t.Errorf("non-recurring clone must run immediately (nil ScheduledFor), got %v", tc.ScheduledFor)
		}
	})

	t.Run("a copy never inherits the source's name", func(t *testing.T) {
		// tasks.name is the import/export identity key and carries a PARTIAL
		// UNIQUE index on non-empty names. Inheriting it means the copy's INSERT
		// collides with the source row that is still in the table, so every
		// re-run of a named task 500s. Both copy modes must clear it — the same
		// rule storage.scheduleNextRecurrence already applies to the next
		// occurrence of a recurring task.
		named := &models.Task{
			Name:       "reklaim-daily-health-scan",
			Prompt:     "do the work for the team",
			Recurrence: "0 9 * * *",
			Timezone:   "UTC",
		}
		for _, keepRecurrence := range []bool{false, true} {
			tc, err := buildRerunTaskCreate(named, keepRecurrence, taskRerunOverrides{}, time.UTC)
			if err != nil {
				t.Fatalf("build(keepRecurrence=%v): %v", keepRecurrence, err)
			}
			if tc.Name != "" {
				t.Errorf("keepRecurrence=%v: copy name = %q, want empty (unique-index collision with the source)", keepRecurrence, tc.Name)
			}
		}
	})

	t.Run("rerun of a gated source cannot bypass the gate", func(t *testing.T) {
		// The security regression behind the RunIf enforcement contract: any
		// create_task principal may rerun an admin-authored gated task, and the
		// rerun's ScheduledFor=nil run-now convention used to mint it PENDING —
		// executing the gated work with the condition never evaluated. The
		// minted task must instead land on the scheduler path, where
		// ProcessScheduledTasks evaluates the gate before promotion.
		gated := &models.Task{
			Prompt:   "do the gated work for the team",
			Timezone: "UTC",
			RunIf:    &models.RunIf{Command: "test -f /tmp/ready", TimeoutSeconds: 30},
		}
		tc, err := buildRerunTaskCreate(gated, false, taskRerunOverrides{}, time.UTC)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if tc.RunIf == nil {
			t.Fatal("rerun recipe must carry the source's run_if")
		}
		task := models.NewTask(tc)
		if task.Status != models.TaskStatusScheduled {
			t.Errorf("gated rerun status = %q, want scheduled (pending would dispatch with the gate unevaluated)", task.Status)
		}
		if task.ScheduledFor == nil {
			t.Error("gated rerun must be parked with a non-nil scheduled_for so the scheduler picks it up")
		}
	})
}

func TestApplyRerunOverrides(t *testing.T) {
	base := func() models.TaskCreate {
		return models.TaskCreate{Prompt: "orig", Priority: 1, Tags: []string{"a"}}
	}

	t.Run("nil overrides leave everything", func(t *testing.T) {
		tc := base()
		applyRerunOverrides(&tc, taskRerunOverrides{})
		if tc.Prompt != "orig" || tc.Priority != 1 || len(tc.Tags) != 1 {
			t.Errorf("empty overrides changed fields: %+v", tc)
		}
	})

	t.Run("set fields override", func(t *testing.T) {
		tc := base()
		newPrompt, newPri := "changed", 9
		applyRerunOverrides(&tc, taskRerunOverrides{Prompt: &newPrompt, Priority: &newPri})
		if tc.Prompt != "changed" || tc.Priority != 9 {
			t.Errorf("overrides not applied: prompt=%q priority=%d", tc.Prompt, tc.Priority)
		}
		if len(tc.Tags) != 1 || tc.Tags[0] != "a" {
			t.Errorf("unset field should be unchanged, got %v", tc.Tags)
		}
	})

	t.Run("nil tags inherit, non-nil tags replace", func(t *testing.T) {
		tc := base()
		applyRerunOverrides(&tc, taskRerunOverrides{}) // nil tags
		if len(tc.Tags) != 1 || tc.Tags[0] != "a" {
			t.Errorf("nil tags should inherit, got %v", tc.Tags)
		}
		tc = base()
		applyRerunOverrides(&tc, taskRerunOverrides{Tags: []string{}}) // explicit empty → replace
		if len(tc.Tags) != 0 {
			t.Errorf("explicit empty tags should clear, got %v", tc.Tags)
		}
	})
}
