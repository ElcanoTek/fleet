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
	h := &Handlers{}
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
}

func TestApplyRerunOverrides(t *testing.T) {
	base := func() models.TaskCreate {
		return models.TaskCreate{Prompt: "orig", Priority: 1, RuntimeFlavor: "native-acp", Tags: []string{"a"}}
	}

	t.Run("nil overrides leave everything", func(t *testing.T) {
		tc := base()
		applyRerunOverrides(&tc, taskRerunOverrides{})
		if tc.Prompt != "orig" || tc.Priority != 1 || tc.RuntimeFlavor != "native-acp" || len(tc.Tags) != 1 {
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
		if tc.RuntimeFlavor != "native-acp" {
			t.Errorf("unset field should be unchanged, got %q", tc.RuntimeFlavor)
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
