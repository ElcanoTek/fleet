package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_no_session_approval matches by suffix exactly like
// critical_tools, so a named-account variant of the same tool is covered too,
// and an undeclared bundle changes nothing.
func TestSessionApprovalAllowed(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if !SessionApprovalAllowed("mcp_deals_create_deal") || !SessionApprovalAllowed("send_email") {
		t.Fatal("with no declaration every tool must keep apply-all")
	}

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:          []string{"create_deal", "deploy_page"},
		CriticalToolNoSessionApproval: []string{" create_deal ", "send_email", ""},
	})
	for _, tool := range []string{"create_deal", "mcp_deals_create_deal", "mcp_deals_client_a_create_deal", "mcp_sendgrid_send_email"} {
		if SessionApprovalAllowed(tool) {
			t.Errorf("%s: apply-all allowed, want refused", tool)
		}
	}
	for _, tool := range []string{"mcp_pages_deploy_page", "mcp_deals_precreate_deals", "mcp_deals_create_deal_preview"} {
		if !SessionApprovalAllowed(tool) {
			t.Errorf("%s: apply-all refused, want allowed (no suffix match)", tool)
		}
	}
}

// A member that is not a critical suffix is reported by the preflight (it
// would be inert at boot: no card is staged for that tool), but still
// installed — refusing a session scope is the safe direction.
func TestNoSessionApprovalProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes:          []string{"create_deal"},
		CriticalToolNoSessionApproval: []string{"create_deal", "send_email", "creat_deal"},
	}
	problems := NoSessionApprovalProblems(p)
	if len(problems) != 1 || !strings.Contains(problems[0], `"creat_deal"`) {
		t.Fatalf("problems = %v, want exactly the typo'd member", problems)
	}
	ConfigureAgentPolicy(p)
	if SessionApprovalAllowed("mcp_x_creat_deal") {
		t.Error("a reported member must still be installed")
	}
	if got := NoSessionApprovalProblems(AgentPolicy{}); len(got) != 0 {
		t.Errorf("no declaration must report nothing, got %v", got)
	}
}
