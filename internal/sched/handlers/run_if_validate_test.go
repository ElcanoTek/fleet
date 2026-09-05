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
	h := newValidateTestHandlers()
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

	t.Run("omitted timeout means the default 30", func(t *testing.T) {
		// timeout_seconds is `omitempty` and documented "default 30", and
		// EffectiveTimeoutSeconds/Normalized already read 0 that way — Validate
		// used to contradict the schema by rejecting the omitted field.
		tc := models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "true"}}
		if err := h.validateTaskCreate(&tc); err != nil {
			t.Fatalf("omitted timeout should be accepted, got %v", err)
		}
		if got := tc.RunIf.EffectiveTimeoutSeconds(); got != 30 {
			t.Fatalf("effective timeout = %d, want the default 30", got)
		}
	})

	t.Run("timeout out of range rejected", func(t *testing.T) {
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, RunIf: &models.RunIf{Command: "true", TimeoutSeconds: -1}})
		if err == nil || !strings.Contains(err.Error(), "run_if") {
			t.Fatalf("timeout=-1 should be rejected with a run_if error, got %v", err)
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

// run_if executes on the HOST as the fleet user, so only an admin principal
// may attach one — structural validation cannot make an arbitrary creator's
// shell string safe.
func TestRequireAdminForRunIf(t *testing.T) {
	gate := &models.RunIf{Command: "true", OnError: models.RunIfOnErrorSkip, TimeoutSeconds: 5}

	if msg := requireAdminForRunIf(false, gate); msg == "" {
		t.Fatal("non-admin with a run_if gate must be rejected")
	}
	if msg := requireAdminForRunIf(true, gate); msg != "" {
		t.Fatalf("admin rejected: %s", msg)
	}
	if msg := requireAdminForRunIf(false, nil); msg != "" {
		t.Fatalf("non-admin without a gate rejected: %s", msg)
	}
}
