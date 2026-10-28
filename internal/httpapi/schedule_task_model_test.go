package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestScheduledTaskModel pins the model a chat-created scheduled task is
// pinned to. An explicit slug from the agent wins. Otherwise the task gets the
// live task tier, NOT the conversation's model: chat runs on the fast everyday
// tier, and a scheduled task is a long unattended run (docs/TASK-MODEL-TIER.md).
func TestScheduledTaskModel(t *testing.T) {
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetTaskModel("")
	})

	if got := scheduledTaskModel("  vendor/pinned  "); got != "vendor/pinned" {
		t.Errorf("model = %q, want the trimmed explicit slug", got)
	}

	agentcore.SetDefaultModel("acme/frontier-pro")
	if got := scheduledTaskModel(""); got != "acme/frontier-pro" {
		t.Errorf("model = %q, want the task tier (following the default tier)", got)
	}

	agentcore.SetTaskModel("acme/long-runner")
	if got := scheduledTaskModel("   "); got != "acme/long-runner" {
		t.Errorf("model = %q, want the task-tier override", got)
	}
}
