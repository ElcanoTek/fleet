package handlers

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateSandboxLimits exercises the #205 bounds against operator ceilings:
// nil / all-zero (use globals) and in-range values pass; sub-floor and
// over-ceiling values are rejected.
func TestValidateSandboxLimits(t *testing.T) {
	h := &Handlers{config: Config{SandboxMemoryMaxMB: 8192, SandboxCPUsMax: 16, SandboxPidsMax: 1024}}

	valid := []*models.TaskSandboxLimits{
		nil,
		{},                                     // all zero = use the global defaults
		{MemoryMB: 2048, CPUs: 2.0, Pids: 512}, // mid-range
		{MemoryMB: 8192, CPUs: 16, Pids: 1024}, // exactly at the ceiling
		{MemoryMB: 128, Pids: 16},              // exactly at the floor
	}
	for _, l := range valid {
		if err := h.validateSandboxLimits(l); err != nil {
			t.Errorf("expected %+v valid, got %v", l, err)
		}
	}

	invalid := []*models.TaskSandboxLimits{
		{MemoryMB: 64},    // below the 128 MiB floor
		{MemoryMB: 16384}, // above the ceiling
		{CPUs: -1},        // negative
		{CPUs: 32},        // above the ceiling
		{Pids: 8},         // below the 16 floor
		{Pids: 4096},      // above the ceiling
	}
	for _, l := range invalid {
		if err := h.validateSandboxLimits(l); err == nil {
			t.Errorf("expected %+v rejected, got nil", l)
		}
	}
}

// TestValidateSandboxLimits_NoCeiling: with the operator maxima unset (0), any
// in-floor value is accepted, but the floors still apply (#205).
func TestValidateSandboxLimits_NoCeiling(t *testing.T) {
	h := &Handlers{config: Config{}} // all *Max == 0 → uncapped
	if err := h.validateSandboxLimits(&models.TaskSandboxLimits{MemoryMB: 1 << 20, CPUs: 999, Pids: 1 << 20}); err != nil {
		t.Errorf("uncapped should accept large values, got %v", err)
	}
	if err := h.validateSandboxLimits(&models.TaskSandboxLimits{MemoryMB: 1}); err == nil {
		t.Error("the 128 MiB floor still applies even with no ceiling")
	}
}
