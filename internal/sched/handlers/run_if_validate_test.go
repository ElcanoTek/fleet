package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_RunIf pins that validateTaskCreate accepts a nil or
// valid run_if gate and rejects a structurally-broken one (#269): empty command,
// out-of-range timeout, and an invalid on_error policy.
func TestValidateTaskCreate_RunIf(t *testing.T) {
	h := &Handlers{}
	prompt := "do the thing for the team"

	t.Run("nil accepted", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("nil run_if should be accepted, got %v", err)
		}
	})

	t.Run("valid accepted", func(t *testing.T) {
		r := &models.RunIf{Command: "true", ExitCodeIs: 0, TimeoutSeconds: 30, OnError: models.RunIfOnErrorRun}
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: r}); err != nil {
			t.Fatalf("valid run_if should be accepted, got %v", err)
		}
	})

	t.Run("empty command rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "  ", TimeoutSeconds: 30}})
		if err == nil || !strings.Contains(err.Error(), "run_if") {
			t.Fatalf("empty command should be rejected with a run_if error, got %v", err)
		}
	})

	t.Run("timeout out of range rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "true", TimeoutSeconds: 0}})
		if err == nil || !strings.Contains(err.Error(), "run_if") {
			t.Fatalf("timeout=0 should be rejected with a run_if error, got %v", err)
		}
		err = h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "true", TimeoutSeconds: 301}})
		if err == nil || !strings.Contains(err.Error(), "run_if") {
			t.Fatalf("timeout=301 should be rejected with a run_if error, got %v", err)
		}
	})

	t.Run("invalid on_error rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "true", OnError: "bogus"}})
		if err == nil || !strings.Contains(err.Error(), "run_if") {
			t.Fatalf("invalid on_error should be rejected with a run_if error, got %v", err)
		}
	})
}
