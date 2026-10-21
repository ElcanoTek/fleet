package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_narrowable_args matches by suffix like critical_tools (the
// longest suffix wins), and a bundle without it narrows nothing.
func TestNarrowableArgFor(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if got := NarrowableArgFor("mcp_ix_execute_plan"); got != "" {
		t.Fatalf("with no declaration nothing is narrowable, got %q", got)
	}

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan", "ix_execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{
			"execute_plan":    " deal_ids ",
			"ix_execute_plan": "item_keys",
		},
	})
	for tool, want := range map[string]string{
		"execute_plan":                  "deal_ids",
		"mcp_pubmatic_execute_plan":     "deal_ids",
		"mcp_ix_execute_plan":           "item_keys", // the longer suffix wins
		"mcp_pubmatic_execute_plans":    "",
		"mcp_pubmatic_execute_plan_dry": "",
	} {
		if got := NarrowableArgFor(tool); got != want {
			t.Errorf("%s: %q, want %q", tool, got, want)
		}
	}
}

// An entry whose key stages no card for an MCP call (not critical, notify
// mode, a handler-only card) or whose argument is not a plain top-level name
// is reported by validate-config and dropped at boot.
func TestNarrowableArgProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan", "deploy_page", "schedule_task", "update_deal", "email", "template_email", "mail_send_email", "bash", "shell_bash"},
		CriticalToolModes:    map[string]string{"deploy_page": "notify"},
		CriticalToolNarrowableArgs: map[string]string{
			"execute_plan":    "deal_ids",
			"execute_plans":   "deal_ids",
			"deploy_page":     "slugs",
			"schedule_task":   "ids",
			"update_deal":     "deal_ids.0",
			"send_email":      "to",
			"email":           "to",
			"template_email":  "to",
			"mail_send_email": "to",
			"bash":            "commands",
			"shell_bash":      "commands",
			"":                "deal_ids",
		},
	}
	problems := NarrowableArgProblems(p)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{`"execute_plans"`, `"deploy_page"`, `"schedule_task"`, `"deal_ids.0"`, "empty tool suffix", `"send_email"`, `"email"`, `"template_email"`, `"mail_send_email"`, `native bash`} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not name %s", joined, want)
		}
	}
	if len(problems) != 10 {
		t.Fatalf("problems = %d (%q), want exactly the ten bad entries", len(problems), joined)
	}

	ConfigureAgentPolicy(p)
	if NarrowableArgFor("mcp_ix_execute_plan") != "deal_ids" {
		t.Fatal("a valid entry must be installed")
	}
	// The native bash tool is never narrowable; an MCP tool named by its
	// server (shell_bash) still is.
	if NarrowableArgFor("bash") != "" || NarrowableArgFor("mcp_shell_bash") != "commands" {
		t.Fatalf("bash = %q, mcp_shell_bash = %q", NarrowableArgFor("bash"), NarrowableArgFor("mcp_shell_bash"))
	}
	for _, tool := range []string{"mcp_pages_deploy_page", "schedule_task", "mcp_ix_update_deal"} {
		if got := NarrowableArgFor(tool); got != "" {
			t.Errorf("%s: %q, want the reported entry dropped at boot", tool, got)
		}
	}
}
