package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_progress matches by suffix like critical_tools, and a bundle
// without it opts nothing in.
func TestApprovalProgress(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if ApprovalProgress("mcp_pubmatic_execute_plan") {
		t.Fatal("with no declaration no call may report progress")
	}
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan"},
		CriticalToolProgress: []string{"execute_plan"},
	})
	for _, tool := range []string{"execute_plan", "mcp_pubmatic_execute_plan", "mcp_magnite_client_a_execute_plan"} {
		if !ApprovalProgress(tool) {
			t.Errorf("%s: no progress, want progress", tool)
		}
	}
	if ApprovalProgress("mcp_pubmatic_execute_plans") || ApprovalProgress("mcp_sendgrid_send_email") {
		t.Error("undeclared tools must not report progress")
	}
}

// A member that stages no card (not critical, or notify mode) is reported and
// dropped at boot.
func TestApprovalProgressProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan", "deploy_page"},
		CriticalToolModes:    map[string]string{"deploy_page": "notify"},
		CriticalToolProgress: []string{"execute_plan", "execute_plans", "deploy_page"},
	}
	problems := ApprovalProgressProblems(p)
	joined := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(joined, `"execute_plans"`) || !strings.Contains(joined, `"deploy_page"`) {
		t.Fatalf("problems = %q, want the two bad members", joined)
	}
	ConfigureAgentPolicy(p)
	if !ApprovalProgress("mcp_x_execute_plan") || ApprovalProgress("mcp_pages_deploy_page") {
		t.Fatal("valid member installed, reported ones dropped")
	}
}
