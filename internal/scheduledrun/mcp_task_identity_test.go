package scheduledrun

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// A scheduled task with no mcp_selection runs on the SHARED, process-wide MCP
// client, whose workspace-armed servers were spawned at boot against the stable
// per-deployment dir. That directory's ledgers hold the markers of every task
// and every chat conversation on the box, keyed only by (ssp, deal_name) —
// nothing attributes a record to a run.
//
// bindTaskMCP must therefore return NO reconciliation workdir on that path.
// Returning the shared dir replayed other tasks' half-finished creates into
// this task's prompt as "the prior process stopped after submitting these
// creates", and since an abandoned marker is only cleared by a matching
// resolution in the same file, it was replayed into every future run forever.
//
// taskIdentityRequested is the signal that this costs the bundle something it
// explicitly asked for (a ${FLEET_TASK_ID}-bearing env), so the run logs it.
func TestTaskIdentityRequested(t *testing.T) {
	base := func(env map[string]string) config.MCPServerConfig {
		return config.MCPServerConfig{Type: "stdio", Command: "python3", Env: env, Enabled: true}
	}

	t.Run("nil cfg", func(t *testing.T) {
		r := &Runner{}
		if r.taskIdentityRequested() {
			t.Error("a runner with no cfg cannot have a catalog asking for identity")
		}
	})

	t.Run("catalog without the token", func(t *testing.T) {
		r := &Runner{cfg: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"plain": base(map[string]string{"API_KEY": "x"}),
			// The workspace token is a DIFFERENT token: a server can want a
			// writable dir without wanting a per-task identity.
			"workspace_only": base(map[string]string{"WORKDIR": agentcore.WorkspaceEnvToken}),
		}}}
		if r.taskIdentityRequested() {
			t.Error("only ${FLEET_TASK_ID} requests a per-task identity")
		}
	})

	t.Run("one server asking is enough", func(t *testing.T) {
		r := &Runner{cfg: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"plain": base(map[string]string{"API_KEY": "x"}),
			"ledgered": base(map[string]string{
				"CUTLASS_RUN_WORKDIR": agentcore.WorkspaceEnvToken,
				"CUTLASS_MOC_TASK_ID": agentcore.TaskIDEnvToken,
			}),
		}}}
		if !r.taskIdentityRequested() {
			t.Error("a catalog referencing ${FLEET_TASK_ID} must be reported")
		}
	})
}

// The reconciliation workdir is the third return of bindTaskMCP and its ONLY
// consumer is AugmentTaskWithCreateReconciliation. Pin the contract that an
// empty workdir is a no-op there, so the selection-less path cannot inject a
// foreign task's markers no matter what the shared ledger contains.
func TestSharedPathReconciliationIsInert(t *testing.T) {
	const prompt = "Create this week's PG records."
	if got := agentcore.AugmentTaskWithCreateReconciliation(prompt, ""); got != prompt {
		t.Errorf("empty workdir must leave the prompt untouched, got:\n%s", got)
	}
}
