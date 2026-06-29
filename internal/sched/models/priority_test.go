package models

import "testing"

// TestNormalizePriority: the zero value (unset) maps to Normal; any explicit
// value is returned unchanged (#230).
func TestNormalizePriority(t *testing.T) {
	if got := NormalizePriority(0); got != PriorityNormal {
		t.Errorf("NormalizePriority(0) = %d, want %d", got, PriorityNormal)
	}
	for _, p := range []int{1, PriorityCritical, PriorityNormal, PriorityBulk, PriorityMax} {
		if got := NormalizePriority(p); got != p {
			t.Errorf("NormalizePriority(%d) = %d, want unchanged", p, got)
		}
	}
}

// TestNewTaskPriorityDefaults: NewTask defaults an unset priority to Normal and
// always starts EffectivePriority equal to the resolved Priority (#230).
func TestNewTaskPriorityDefaults(t *testing.T) {
	unset := NewTask(TaskCreate{Prompt: "x"})
	if unset.Priority != PriorityNormal || unset.EffectivePriority != PriorityNormal {
		t.Errorf("unset priority = (%d,%d), want (%d,%d)", unset.Priority, unset.EffectivePriority, PriorityNormal, PriorityNormal)
	}

	set := NewTask(TaskCreate{Prompt: "x", Priority: PriorityCritical})
	if set.Priority != PriorityCritical || set.EffectivePriority != PriorityCritical {
		t.Errorf("explicit priority = (%d,%d), want (%d,%d)", set.Priority, set.EffectivePriority, PriorityCritical, PriorityCritical)
	}
}
