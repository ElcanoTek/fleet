package handlers

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_UnpinnedTaskUsesTaskTier pins what replaced the #1014
// create-time refusal. That gate refused a task with neither its own model nor
// a deployment default, because the dispatcher would dead-letter it on the
// first attempt. The task tier made that state impossible: unset, it follows
// the advanced tier, which is never empty. So an unpinned task is accepted,
// and the slug it will run on (and be estimated against) is the live tier.
func TestValidateTaskCreate_UnpinnedTaskUsesTaskTier(t *testing.T) {
	const prompt = "do the thing for the team"
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetAdvancedModel("")
		agentcore.SetTaskModel("")
	})

	t.Run("no model and no fixed default is accepted", func(t *testing.T) {
		h := &Handlers{}
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("an unpinned task must be accepted: the task tier always has a model: %v", err)
		}
	})

	t.Run("the default follows the live task tier", func(t *testing.T) {
		h := &Handlers{config: Config{DefaultTaskModel: "   "}}
		agentcore.SetDefaultModel("acme/frontier-pro")
		if got := h.defaultTaskModel(); got != "acme/frontier-pro" {
			t.Fatalf("default task model = %q, want the default frontier tier it follows", got)
		}
		agentcore.SetTaskModel("acme/long-runner")
		if got := h.defaultTaskModel(); got != "acme/long-runner" {
			t.Fatalf("default task model = %q, want the task-tier override", got)
		}
	})

	t.Run("a fixed default wins", func(t *testing.T) {
		h := &Handlers{config: Config{DefaultTaskModel: "vendor/fixed"}}
		if got := h.defaultTaskModel(); got != "vendor/fixed" {
			t.Fatalf("default task model = %q, want the fixed slug", got)
		}
	})
}
