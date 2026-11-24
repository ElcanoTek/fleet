package agentcore

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMultiAccountSection(t *testing.T) {
	t.Parallel()
	full := toolRoster{toolsByServer: map[string]int{
		"ix_mcp": 3, "ix_mcp_twc": 3, "ox_mcp_arena": 2, "ox_mcp_twc": 2, "email": 1,
	}}

	// One account per server: no section.
	for name, sel := range map[string]MCPSelection{
		"nothing":                 nil,
		"one account each":        {{Server: "email"}, {Server: "ix_mcp", Account: "twc"}},
		"the same account twice":  {{Server: "ix_mcp", Account: "twc"}, {Server: "ix_mcp", Account: "twc"}},
		"two spellings of one":    {{Server: "ix_mcp", Account: "client-a"}, {Server: "ix_mcp", Account: "client_a"}},
		"an entry with no server": {{Account: "twc"}, {Account: "arena"}},
	} {
		if got := full.multiAccountSection(sel); got != "" {
			t.Errorf("%s: got a section:\n%s", name, got)
		}
	}

	sel := MCPSelection{
		{Server: "ox_mcp", Account: "arena"},
		{Server: "ox_mcp", Account: "twc"},
		{Server: "email"},
		{Server: "ix_mcp"},
		{Server: "ix_mcp", Account: "twc"},
	}
	got := full.multiAccountSection(sel)
	for _, want := range []string{
		multiAccountHeading + "\n\n",
		"- `ix_mcp`: the default account (tools `mcp_ix_mcp_*` with no account in the name); account `twc` (tools `mcp_ix_mcp_twc_*`)\n",
		"- `ox_mcp`: account `arena` (tools `mcp_ox_mcp_arena_*`); account `twc` (tools `mcp_ox_mcp_twc_*`)\n",
		"ask before acting",
		"one call per account, each with its own approval",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "email") {
		t.Errorf("a one-account connector is listed:\n%s", got)
	}
	if strings.Contains(got, "NOT available") {
		t.Errorf("every account has tools, yet one is marked unavailable:\n%s", got)
	}
	if strings.Index(got, "`ix_mcp`") > strings.Index(got, "`ox_mcp`") {
		t.Errorf("connectors are not sorted by name:\n%s", got)
	}
	// Byte-stable whatever order the servers come in; an account keeps its
	// place among its own server's accounts.
	reordered := MCPSelection{sel[3], sel[4], sel[2], sel[0], sel[1]}
	if again := full.multiAccountSection(reordered); again != got {
		t.Errorf("the section depends on server order:\n%s\n---\n%s", got, again)
	}
}

// An account with no tool in the roster is offered no tool prefix, and the
// model is told to say so rather than use another account.
func TestMultiAccountSectionLeavesOutAnAccountWithNoTools(t *testing.T) {
	t.Parallel()
	roster := toolRoster{toolsByServer: map[string]int{"ix_mcp": 3, "ix_mcp_arena": 3}}
	got := roster.multiAccountSection(MCPSelection{
		{Server: "ix_mcp"}, {Server: "ix_mcp", Account: "twc"}, {Server: "ix_mcp", Account: "arena"},
	})
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
		t.Errorf("the section offers the tool prefix of an account with no tools:\n%s", got)
	}
}

// The section reads the roster the gates produced, so it cannot disagree with
// the live registry beside it: an account whose every tool
// interactive_hidden_tools removed is not available, whether the roster is
// registered directly or deferred behind the bridges.
func TestMultiAccountSectionFollowsTheGatedRoster(t *testing.T) {
	tool := func(server, name string) mcp.ServerTool {
		return mcp.ServerTool{ServerName: server, Tool: mcp.Tool{
			Name: name, Description: "A tool.", InputSchema: map[string]interface{}{"type": "object"},
		}}
	}
	catalog := []mcp.ServerTool{
		tool("ix", "get_deal"), tool("ix", "write_deal"),
		// The twc account exports only the tool chat does not offer.
		tool("ix_twc", "write_deal"),
		tool("ix_arena", "get_deal"),
	}
	selection := MCPSelection{{Server: "ix"}, {Server: "ix", Account: "twc"}, {Server: "ix", Account: "arena"}}
	hidden, problems := buildInteractiveHiddenList([]string{"write_deal"})
	if len(problems) != 0 {
		t.Fatalf("problems = %q", problems)
	}

	for name, threshold := range map[string]string{"direct": "100", "deferred": "2"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FLEET_TOOL_DISCLOSURE_THRESHOLD", threshold)
			_, roster, err := buildFantasyTools(context.Background(), discNative(3), catalog, &fakeBroker{}, nil, nil, nil, nil,
				toolBuildConfig{hiddenSuffixes: hidden})
			if err != nil {
				t.Fatal(err)
			}
			if (name == "deferred") != (roster.deferredMCP > 0) {
				t.Fatalf("deferredMCP = %d in the %s case", roster.deferredMCP, name)
			}
			got := roster.multiAccountSection(selection)
			for _, want := range []string{
				"the default account (tools `mcp_ix_*` with no account in the name)",
				"account `twc` (NOT available this turn: no tools)",
				"account `arena` (tools `mcp_ix_arena_*`)",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("section lacks %q:\n%s", want, got)
				}
			}
			// With nothing hidden the same account has its tool again.
			_, open, err := buildFantasyTools(context.Background(), discNative(3), catalog, &fakeBroker{}, nil, nil, nil, nil, toolBuildConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if section := open.multiAccountSection(selection); !strings.Contains(section, "account `twc` (tools `mcp_ix_twc_*`)") {
				t.Errorf("with no hidden list the twc account should be available:\n%s", section)
			}
		})
	}
}

func TestWithRosterSections(t *testing.T) {
	t.Parallel()
	roster := toolRoster{
		directMCP:     []string{"mcp_ix_get_deal", "mcp_ix_twc_get_deal"},
		toolsByServer: map[string]int{"ix": 1, "ix_twc": 1},
	}
	two := MCPSelection{{Server: "ix"}, {Server: "ix", Account: "twc"}}

	// The driver did not ask (a scheduled run), or there is nothing to say:
	// the prompt is exactly the live-registry form it always was.
	plain := withLiveRegistry("base\n", roster)
	if got := withRosterSections("base\n", roster, RunConfig{Selection: two}); got != plain {
		t.Errorf("no MultiAccountNotice changed the prompt:\n%s", got)
	}
	if got := withRosterSections("base\n", roster, RunConfig{MultiAccountNotice: true, Selection: two[:1]}); got != plain {
		t.Errorf("a one-account selection changed the prompt:\n%s", got)
	}

	got := withRosterSections("base\n\n", roster, RunConfig{MultiAccountNotice: true, Selection: two})
	notice, registry := strings.Index(got, multiAccountHeading), strings.Index(got, liveRegistryHeading)
	if !strings.HasPrefix(got, "base\n\n"+multiAccountHeading) || notice < 0 || registry < notice {
		t.Errorf("want base, then the multi-account section, then the live registry last:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("sections are not joined by exactly one blank line:\n%q", got)
	}
	if got := withRosterSections("", roster, RunConfig{MultiAccountNotice: true, Selection: two}); !strings.HasPrefix(got, multiAccountHeading) {
		t.Errorf("an empty base should start with the section:\n%s", got)
	}
}
