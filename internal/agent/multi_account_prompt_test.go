package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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
	render := func(enabled []string, defaults map[string]string, extra map[string][]string) string {
		var sb strings.Builder
		writeMultiAccountNotice(&sb, multiAccountConnectors(enabled, defaults, extra))
		return sb.String()
	}

	if got := render([]string{"ix_mcp"}, map[string]string{"ix_mcp": "twc"}, nil); got != "" {
		t.Fatalf("a one-account turn got a section:\n%s", got)
	}
	// An extra that only repeats the first account is still one account.
	if got := render([]string{"ix_mcp"}, map[string]string{"ix_mcp": "twc"}, map[string][]string{"ix_mcp": {"twc"}}); got != "" {
		t.Fatalf("a repeated account got a section:\n%s", got)
	}
	// A connector that is not on this turn is not described.
	if got := render([]string{"email"}, nil, map[string][]string{"ix_mcp": {"twc"}}); got != "" {
		t.Fatalf("a connector that is off got a section:\n%s", got)
	}

	got := render(
		[]string{"ox_mcp", "ix_mcp", "email"},
		map[string]string{"ox_mcp": "arena"},
		map[string][]string{"ix_mcp": {"twc"}, "ox_mcp": {"twc"}},
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
	if strings.Index(got, "`ix_mcp`") > strings.Index(got, "`ox_mcp`") {
		t.Errorf("connectors are not sorted by name:\n%s", got)
	}
	// Byte-stable for the same choice (docs/PROMPT-CACHE-CONTRACT.md).
	again := render(
		[]string{"email", "ix_mcp", "ox_mcp"},
		map[string]string{"ox_mcp": "arena"},
		map[string][]string{"ox_mcp": {"twc"}, "ix_mcp": {"twc"}},
	)
	if again != got {
		t.Errorf("the section depends on the order connectors were enabled in:\n%s\n---\n%s", got, again)
	}
}
