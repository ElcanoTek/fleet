package models

import "testing"

// TestTaskSandboxLimits_IsZero: nil and an all-zero struct both read as "no
// override"; any set field makes it non-zero (#205).
func TestTaskSandboxLimits_IsZero(t *testing.T) {
	var nilLimits *TaskSandboxLimits
	if !nilLimits.IsZero() {
		t.Error("nil limits should be zero")
	}
	if !(&TaskSandboxLimits{}).IsZero() {
		t.Error("empty limits should be zero")
	}
	for _, l := range []*TaskSandboxLimits{
		{MemoryMB: 2048},
		{CPUs: 2.0},
		{Pids: 512},
	} {
		if l.IsZero() {
			t.Errorf("limits %+v should be non-zero", l)
		}
	}
}

// TestNewTask_SandboxLimitsPropagation: NewTask carries SandboxLimits from the
// create recipe onto the Task verbatim (nil stays nil) (#205).
func TestNewTask_SandboxLimitsPropagation(t *testing.T) {
	if got := NewTask(TaskCreate{Prompt: "x"}); got.SandboxLimits != nil {
		t.Errorf("unset SandboxLimits should be nil, got %+v", got.SandboxLimits)
	}
	lim := &TaskSandboxLimits{MemoryMB: 2048, CPUs: 2.0, Pids: 512}
	got := NewTask(TaskCreate{Prompt: "x", SandboxLimits: lim})
	if got.SandboxLimits == nil || *got.SandboxLimits != *lim {
		t.Errorf("SandboxLimits = %+v, want %+v", got.SandboxLimits, lim)
	}
}
