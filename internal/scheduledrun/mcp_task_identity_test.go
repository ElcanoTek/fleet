package scheduledrun

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The reconciliation workdir is the third return of bindTaskMCP and its ONLY
// consumer is AugmentTaskWithCreateReconciliation. Pin the contract that an
// empty workdir remains a no-op for tasks whose selected connector set does not
// request a workspace.
func TestEmptyReconciliationWorkdirIsInert(t *testing.T) {
	const prompt = "Create this week's PG records."
	if got := agentcore.AugmentTaskWithCreateReconciliation(prompt, ""); got != prompt {
		t.Errorf("empty workdir must leave the prompt untouched, got:\n%s", got)
	}
}
