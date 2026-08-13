package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_ModelGate pins the create-time refusal from #1014.
//
// A task with neither its own model nor a deployment default can never run: the
// dispatcher resolves the model first, fails terminally, and dead-letters the task
// on attempt 1 having executed nothing. Before this gate such a task was accepted,
// displayed as healthy in the Operations Center, and only revealed itself up to a
// cron period later in the DLQ — which is exactly how every Pages autoupdate task
// died. Refusing at create time turns a silent delayed failure into an immediate one.
func TestValidateTaskCreate_ModelGate(t *testing.T) {
	const prompt = "do the thing for the team"
	model := "vendor/some-model"

	t.Run("no task model and no deployment default is refused", func(t *testing.T) {
		h := &Handlers{}
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt})
		if err == nil {
			t.Fatal("a task that can never run should be refused at create time")
		}
		// The message has to tell an operator which knob to set — the whole point
		// is that they see it here instead of decoding a DLQ entry later.
		if !strings.Contains(err.Error(), "FLEET_TASK_MODEL") {
			t.Errorf("error should name the env knob to set, got %v", err)
		}
	})

	t.Run("task's own model is enough", func(t *testing.T) {
		h := &Handlers{}
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Model: &model}); err != nil {
			t.Fatalf("a task pinning its own model must be accepted with no deployment default: %v", err)
		}
	})

	t.Run("deployment default is enough", func(t *testing.T) {
		h := newValidateTestHandlers()
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("a task with no model must be accepted when the deployment has a default: %v", err)
		}
	})

	t.Run("a whitespace-only deployment default does not count", func(t *testing.T) {
		h := &Handlers{config: Config{DefaultTaskModel: "   "}}
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err == nil {
			t.Fatal("a blank default is no default; the task still cannot run")
		}
	})
}
