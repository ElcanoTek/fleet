package agentcore

import "charm.land/fantasy"

// Test-only hooks for the external agentcore_test package, which can import
// clientconfig (clientconfig imports agentcore, so an internal test cannot).

// ConfirmAuditToolForTest returns the confirm_audit tool wired to policy, as
// the scheduled tool roster registers it.
func ConfirmAuditToolForTest(policy Policy) fantasy.AgentTool {
	return buildConfirmAuditPolicyTool(policy)
}

// RestoreTestFixturePolicy reinstalls the agent policy TestMain installs.
func RestoreTestFixturePolicy() { ConfigureAgentPolicy(testFixturePolicy()) }
