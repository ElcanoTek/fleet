package agentcore

import (
	"strings"
	"testing"
)

func TestHostReadProblems(t *testing.T) {
	t.Parallel()
	ok := AgentPolicy{ParallelSafeTools: []string{"mcp_deals_list_account_groups"}, CriticalToolSuffixes: []string{"update_deal"}}
	if got := HostReadProblems(ok, nil, "account_groups.source", "deals", "list_account_groups"); len(got) != 0 {
		t.Fatalf("a declared read reported problems: %q", got)
	}
	// Not declared a read.
	got := HostReadProblems(AgentPolicy{}, nil, "account_groups.source", "deals", "list_account_groups")
	if len(got) != 1 || !strings.Contains(got[0], "not in agent_policy.parallel_safe_tools") || !strings.Contains(got[0], "account_groups.source") {
		t.Fatalf("problems = %q", got)
	}
	// Listed as a read, but critical under the bundle's suffixes: both the
	// bundle's own and fleet's base email suffixes count.
	critical := AgentPolicy{ParallelSafeTools: []string{"mcp_deals_update_deal"}, CriticalToolSuffixes: []string{"update_deal"}}
	if got := HostReadProblems(critical, nil, "k", "deals", "update_deal"); len(got) != 1 || !strings.Contains(got[0], "critical tool") {
		t.Fatalf("critical: problems = %q", got)
	}
	base := AgentPolicy{ParallelSafeTools: []string{"mcp_mail_send_email"}}
	if got := HostReadProblems(base, nil, "k", "mail", "send_email"); len(got) != 1 || !strings.Contains(got[0], "critical tool") {
		t.Fatalf("base critical suffix: problems = %q", got)
	}
	// A pre_tool_use hook that matches the tool: the call would be skipped at
	// run time, so the preflight says so too. Other events and other tools'
	// hooks do not count.
	hooks := []LifecycleHook{
		{ID: "post", Event: HookPostToolUse, Matcher: "*"},
		{ID: "other", Event: HookPreToolUse, Matcher: "mcp_ix_*"},
	}
	if got := HostReadProblems(ok, hooks, "k", "deals", "list_account_groups"); len(got) != 0 {
		t.Fatalf("hooks that do not match reported problems: %q", got)
	}
	matching := []LifecycleHook{hooks[0], hooks[1], {ID: "audit-deals", Event: HookPreToolUse, Matcher: "mcp_deals_*"}}
	if got := HostReadProblems(ok, matching, "k", "deals", "list_account_groups"); len(got) != 1 || !strings.Contains(got[0], `pre_tool_use hook "audit-deals"`) {
		t.Fatalf("matching hook: problems = %q", got)
	}
}
