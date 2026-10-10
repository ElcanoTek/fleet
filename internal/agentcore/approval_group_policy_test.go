package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_group_approval matches by suffix like critical_tools, and a
// bundle without it groups nothing.
func TestGroupsApproval(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if GroupsApproval("mcp_pubmatic_execute_plan") || GroupsApproval("send_email") {
		t.Fatal("with no declaration no card may join a group")
	}

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:      []string{"execute_plan", "deploy_page"},
		CriticalToolGroupApproval: []string{" execute_plan ", ""},
	})
	for _, tool := range []string{"execute_plan", "mcp_pubmatic_execute_plan", "mcp_magnite_client_a_execute_plan"} {
		if !GroupsApproval(tool) {
			t.Errorf("%s: not grouped, want grouped", tool)
		}
	}
	for _, tool := range []string{"mcp_pages_deploy_page", "mcp_pubmatic_execute_plans", "mcp_pubmatic_execute_plan_preview", "mcp_sendgrid_send_email"} {
		if GroupsApproval(tool) {
			t.Errorf("%s: grouped, want not grouped (not declared)", tool)
		}
	}
}

// A member that is not critical (no card is ever staged), or that runs in
// notify mode (no card either), is reported by validate-config and dropped at
// boot. A base email suffix is critical, so it passes.
func TestGroupApprovalProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes:      []string{"execute_plan", "deploy_page"},
		CriticalToolModes:         map[string]string{"deploy_page": "notify"},
		CriticalToolGroupApproval: []string{"execute_plan", "send_email", "execute_plans", "deploy_page", "schedule_task"},
	}
	p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, "schedule_task")
	problems := GroupApprovalProblems(p)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`"execute_plans"`, `"deploy_page"`, `"schedule_task"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not name %s", joined, want)
		}
	}
	if len(problems) != 3 {
		t.Fatalf("problems = %d (%q), want exactly the three bad members", len(problems), joined)
	}

	ConfigureAgentPolicy(p)
	if !GroupsApproval("mcp_pubmatic_execute_plan") || !GroupsApproval("mcp_sendgrid_send_email") {
		t.Fatal("valid members must be installed")
	}
	if GroupsApproval("mcp_pages_deploy_page") || GroupsApproval("execute_plans") {
		t.Fatal("reported members must be dropped at boot")
	}
}
