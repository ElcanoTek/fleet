package handlers

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

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
