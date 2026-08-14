package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_SLA pins that every create path (create, edit,
// import, estimate all funnel through validateTaskCreate) rejects a
// statically-broken SLA config (#274) instead of persisting one that fires
// spurious alerts at runtime.
func TestValidateTaskCreate_SLA(t *testing.T) {
	h := newValidateTestHandlers()
	prompt := "do the thing for the team"
	expected := 30

	t.Run("no SLA accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("task without an SLA should be accepted, got %v", err)
		}
	})

	t.Run("valid SLA accepted", func(t *testing.T) {
		tc := models.TaskCreate{Prompt: prompt, ExpectedDurationMinutes: &expected, SLAWarnMultiplier: 1.2, SLAFailMultiplier: 3}
		if err := h.validateTaskCreate(&tc); err != nil {
			t.Fatalf("valid SLA should be accepted, got %v", err)
		}
	})

	t.Run("non-positive expected duration rejected", func(t *testing.T) {
		zero := 0
		tc := models.TaskCreate{Prompt: prompt, ExpectedDurationMinutes: &zero}
		if err := h.validateTaskCreate(&tc); err == nil || !strings.Contains(err.Error(), "expected_duration_minutes") {
			t.Fatalf("zero expected duration should be rejected, got %v", err)
		}
	})

	t.Run("fail at or below warn rejected", func(t *testing.T) {
		tc := models.TaskCreate{Prompt: prompt, ExpectedDurationMinutes: &expected, SLAWarnMultiplier: 2, SLAFailMultiplier: 1.5}
		if err := h.validateTaskCreate(&tc); err == nil || !strings.Contains(err.Error(), "must exceed") {
			t.Fatalf("fail multiplier below warn should be rejected, got %v", err)
		}
	})
}
