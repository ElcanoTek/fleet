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

// critical_tool_card_describers keeps an entry only when the key is critical
// and the describer is a parallel-safe, non-critical tool. A critical
// describer would run a write without approval, so it is always dropped.
func TestCardDescribers(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"update_deal", "create_deal", "deploy_page", "deal"},
		ParallelSafeTools:    []string{"mcp_deals_describe_update", "mcp_pages_preview_page", "mcp_deals_describe_deal"},
		CriticalToolCardDescribers: map[string]string{
			"update_deal":  "describe_update", // ok
			"deploy_page":  "preview_page",    // ok
			"create_deal":  "describe_deal",   // critical: ends in _deal
			"publish_page": "preview_page",    // key is not critical
			"deal":         "describe_missing",
		},
	}
	// The suffix rule makes a describer named after its critical tool
	// critical itself: describe_update_deal ends in _update_deal.
	if got := CardDescriberProblems(AgentPolicy{
		CriticalToolSuffixes:       []string{"update_deal"},
		ParallelSafeTools:          []string{"mcp_deals_describe_update_deal"},
		CriticalToolCardDescribers: map[string]string{"update_deal": "describe_update_deal"},
	}); len(got) != 1 || !strings.Contains(got[0], "is a critical tool") {
		t.Errorf("describe_update_deal must be refused as critical, got %v", got)
	}
	problems := strings.Join(CardDescriberProblems(p), "\n")
	for _, want := range []string{`"create_deal"`, `"publish_page"`, `"describe_missing" is not in parallel_safe_tools`} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems lack %s:\n%s", want, problems)
		}
	}
	if strings.Contains(problems, `"update_deal"`) || strings.Contains(problems, `"deploy_page"`) {
		t.Errorf("valid entries reported:\n%s", problems)
	}
	ConfigureAgentPolicy(p)
	if got := CardDescriberFor("mcp_deals_client_a_update_deal"); got != "describe_update" {
		t.Errorf("update_deal describer = %q", got)
	}
	if got := CardDescriberFor("mcp_deals_create_deal"); got != "" {
		t.Errorf("a critical describer was installed: %q", got)
	}
	if got := CardDescriberFor("mcp_x_other"); got != "" {
		t.Errorf("an undeclared tool got %q", got)
	}
	ConfigureAgentPolicy(AgentPolicy{})
	if got := CardDescriberFor("mcp_deals_update_deal"); got != "" {
		t.Errorf("no declaration must describe nothing, got %q", got)
	}
}
