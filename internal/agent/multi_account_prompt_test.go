package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestInteractiveRunSelectionSeveralAccounts(t *testing.T) {
	t.Parallel()
	got := interactiveRunSelection(
		[]string{"email", "ix_mcp"},
		map[string]string{"ix_mcp": "twc"},
		// "twc" again is the first account repeated; "TWC" is a different
		// account name, and the binder decides what to do with that one.
		map[string][]string{"ix_mcp": {"", "twc", "arena"}, "not_enabled": {"x"}},
	)
	want := agentcore.MCPSelection{
		{Server: "email"},
		{Server: "ix_mcp", Account: "twc"},
		{Server: "ix_mcp"},
		{Server: "ix_mcp", Account: "arena"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %#v, want %#v", got, want)
	}
	// No extras: the selection is what it always was.
	got = interactiveRunSelection([]string{"email", "ix_mcp"}, map[string]string{"ix_mcp": "twc"}, nil)
	if want := (agentcore.MCPSelection{{Server: "email"}, {Server: "ix_mcp", Account: "twc"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("single selection = %#v, want %#v", got, want)
	}
}

func TestMultiAccountNotice(t *testing.T) {
	t.Parallel()
	catalogOf := func(registered ...string) []mcp.ServerTool {
		out := make([]mcp.ServerTool, 0, len(registered))
		for _, name := range registered {
			out = append(out, mcp.ServerTool{ServerName: name, Tool: mcp.Tool{Name: "get_deal"}})
		}
		return out
	}
	all := catalogOf("ix_mcp", "ix_mcp_twc", "ox_mcp_arena", "ox_mcp_twc", "email")
	render := func(enabled []string, defaults map[string]string, extra map[string][]string, catalog []mcp.ServerTool) string {
		var sb strings.Builder
		writeMultiAccountNotice(&sb, multiAccountConnectors(enabled, defaults, extra, catalog))
		return sb.String()
	}

	if got := render([]string{"ix_mcp"}, map[string]string{"ix_mcp": "twc"}, nil, all); got != "" {
		t.Fatalf("a one-account turn got a section:\n%s", got)
	}
	// An extra that only repeats the first account is still one account.
	if got := render([]string{"ix_mcp"}, map[string]string{"ix_mcp": "twc"}, map[string][]string{"ix_mcp": {"twc"}}, all); got != "" {
		t.Fatalf("a repeated account got a section:\n%s", got)
	}
	// A connector that is not on this turn is not described.
	if got := render([]string{"email"}, nil, map[string][]string{"ix_mcp": {"twc"}}, all); got != "" {
		t.Fatalf("a connector that is off got a section:\n%s", got)
	}

	got := render(
		[]string{"ox_mcp", "ix_mcp", "email"},
		map[string]string{"ox_mcp": "arena"},
		map[string][]string{"ix_mcp": {"twc"}, "ox_mcp": {"twc"}},
		all,
	)
	for _, want := range []string{
		"## Connectors on for several accounts in this chat",
		"- `ix_mcp`: the default account (tools `mcp_ix_mcp_*` with no account in the name); account `twc` (tools `mcp_ix_mcp_twc_*`)",
		"- `ox_mcp`: account `arena` (tools `mcp_ox_mcp_arena_*`); account `twc` (tools `mcp_ox_mcp_twc_*`)",
		"ask before acting",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "email") {
		t.Errorf("a one-account connector is listed:\n%s", got)
	}
	if strings.Contains(got, "NOT available") {
		t.Errorf("every account mounted, yet one is marked unavailable:\n%s", got)
	}
	if strings.Index(got, "`ix_mcp`") > strings.Index(got, "`ox_mcp`") {
		t.Errorf("connectors are not sorted by name:\n%s", got)
	}
	// Byte-stable for the same choice (docs/PROMPT-CACHE-CONTRACT.md).
	again := render(
		[]string{"email", "ix_mcp", "ox_mcp"},
		map[string]string{"ox_mcp": "arena"},
		map[string][]string{"ox_mcp": {"twc"}, "ix_mcp": {"twc"}},
		all,
	)
	if again != got {
		t.Errorf("the section depends on the order connectors were enabled in:\n%s\n---\n%s", got, again)
	}
}

// An account is a best-effort server: it can fail to connect while the
// connector's other accounts come up. The section then offers no tool prefix
// for it and tells the model to say so, never to use another account instead.
func TestMultiAccountNoticeLeavesOutAnAccountThatDidNotMount(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	writeMultiAccountNotice(&sb, multiAccountConnectors(
		[]string{"ix_mcp"}, nil, map[string][]string{"ix_mcp": {"twc", "arena"}},
		[]mcp.ServerTool{
			{ServerName: "ix_mcp", Tool: mcp.Tool{Name: "get_deal"}},
			{ServerName: "ix_mcp_arena", Tool: mcp.Tool{Name: "get_deal"}},
		},
	))
	got := sb.String()
	for _, want := range []string{
		"the default account (tools `mcp_ix_mcp_*` with no account in the name)",
		"account `twc` (NOT available this turn: no tools)",
		"account `arena` (tools `mcp_ix_mcp_arena_*`)",
		"Never do its work on another account.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "mcp_ix_mcp_twc_") {
		t.Errorf("the section offers the tool prefix of an account that did not mount:\n%s", got)
	}
}
