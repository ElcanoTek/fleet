package agentcore

import "testing"

// TestApprovalTimeoutForTool covers the per-tool approval-timeout lookup (#225):
// suffix matching mirrors isCriticalTool, the longest matching suffix wins, and
// unconfigured tools / non-positive entries resolve to 0 ("fall back to the
// per-conversation / global timeout").
func TestApprovalTimeoutForTool(t *testing.T) {
	// Restore the generic default policy after mutating global state.
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolTimeouts: map[string]int{
			"send_email":   120,
			"deal":         60,
			"execute_deal": 90,
			"bash":         45,
			"ignored_zero": 0,  // filtered out (non-positive)
			"":             10, // filtered out (empty key)
		},
	})

	cases := []struct {
		name string
		tool string
		want int
	}{
		{"prefixed send_email matches by suffix", "mcp_sendgrid_send_email", 120},
		{"longest suffix wins over shorter", "mcp_x_execute_deal", 90},
		{"shorter suffix when longer absent", "mcp_x_create_deal", 60},
		{"native tool exact match", "bash", 45},
		{"unconfigured tool falls through", "run_python", 0},
		{"zero-valued entry ignored", "mcp_x_ignored_zero", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApprovalTimeoutForTool(tc.tool); got != tc.want {
				t.Errorf("ApprovalTimeoutForTool(%q) = %d, want %d", tc.tool, got, tc.want)
			}
		})
	}
}

// TestApprovalTimeoutForTool_NoTimeoutsConfigured confirms that a policy which
// declares critical tools but no critical_tool_timeouts yields no per-tool
// override (every tool resolves to 0, so callers fall back to the
// per-conversation / global window).
func TestApprovalTimeoutForTool_NoTimeoutsConfigured(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"send_email"}})
	if got := ApprovalTimeoutForTool("mcp_sendgrid_send_email"); got != 0 {
		t.Errorf("with no critical_tool_timeouts, ApprovalTimeoutForTool = %d, want 0", got)
	}
}
