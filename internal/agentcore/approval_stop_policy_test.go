package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_stop matches by suffix like critical_tools; a member that
// stages no card for an MCP call is reported and dropped at boot.
func TestApprovalStopPolicy(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	ConfigureAgentPolicy(AgentPolicy{})
	if ApprovalStop("mcp_ix_execute_plan") {
		t.Fatal("with no declaration nothing offers Stop")
	}
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan", "deploy_page", "schedule_task"},
		CriticalToolModes:    map[string]string{"deploy_page": "notify"},
		CriticalToolStop:     []string{"execute_plan", "execute_plans", "deploy_page", "schedule_task"},
	}
	problems := ApprovalStopProblems(p)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`"execute_plans"`, `"deploy_page"`, `"schedule_task"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not name %s", joined, want)
		}
	}
	if len(problems) != 3 {
		t.Fatalf("problems = %d (%q), want the three bad members", len(problems), joined)
	}
	ConfigureAgentPolicy(p)
	for tool, want := range map[string]bool{
		"mcp_ix_execute_plan":               true,
		"mcp_magnite_client_a_execute_plan": true,
		"mcp_pages_deploy_page":             false,
		"schedule_task":                     false,
		"mcp_ix_execute_plans":              false,
	} {
		if got := ApprovalStop(tool); got != want {
			t.Errorf("%s: %v, want %v", tool, got, want)
		}
	}
}

// A member whose mode resolves to notify through a shorter overlapping entry
// (plan: notify over execute_plan) stages no card either, so it is reported;
// a longer approve entry wins back, as at run time.
func TestApprovalStopProblems_ResolvesOverlappingModes(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"plan", "execute_plan"},
		CriticalToolModes:    map[string]string{"plan": "notify"},
		CriticalToolStop:     []string{"execute_plan"},
	}
	if problems := ApprovalStopProblems(p); len(problems) != 1 || !strings.Contains(problems[0], `entry "plan"`) {
		t.Fatalf("problems = %q, want execute_plan reported through plan: notify", problems)
	}
	p.CriticalToolModes["execute_plan"] = "approve"
	if problems := ApprovalStopProblems(p); len(problems) != 0 {
		t.Fatalf("problems = %q, want none once execute_plan resolves to approve", problems)
	}
}

// A member naming the native bash tool is reported and dropped: bash runs
// inline, never as an MCP request, so its running card could never offer
// Stop. An MCP tool named by its server (shell_bash) still offers it.
func TestApprovalStopPolicy_RefusesNativeBash(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"bash", "shell_bash", "mcp__http_publish", "_a2a_helpdesk_send"},
		CriticalToolStop:     []string{"bash", "shell_bash", "mcp__http_publish", "_a2a_helpdesk_send"},
	}
	problems := ApprovalStopProblems(p)
	joined := strings.Join(problems, "\n")
	if len(problems) != 3 || !strings.Contains(joined, "native bash") || !strings.Contains(joined, `"mcp__http_publish"`) || !strings.Contains(joined, `"_a2a_helpdesk_send"`) {
		t.Fatalf("problems = %q, want native bash and both synthetic-server members reported", problems)
	}
	ConfigureAgentPolicy(p)
	if ApprovalStop("bash") {
		t.Fatal("native bash must never offer Stop")
	}
	if !ApprovalStop("mcp_shell_bash") {
		t.Fatal("an MCP tool named by its server must still offer Stop")
	}
}
