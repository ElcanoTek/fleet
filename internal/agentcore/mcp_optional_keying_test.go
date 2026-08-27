package agentcore

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestOptionalServerForKeyingRule pins the ONE server-name keying rule (#1272)
// for the Gate-1 Optional set: an exact key wins, otherwise the LONGEST key the
// registered name extends across an underscore wins, and a name that extends no
// key is not Optional at all. Same table shape as TestMCPAllowlistToolsFor,
// because it is the same rule — both resolve through longestServerKey.
func TestOptionalServerForKeyingRule(t *testing.T) {
	optional := mcpOptionalSet{
		"jira":      true,
		"jira_prod": true,
		"srv":       true,
		"declared":  false, // present but not Optional: governs nothing
	}
	cases := []struct {
		registered string
		want       string
	}{
		{"jira", "jira"},              // default seat, own key
		{"jira_prod", "jira_prod"},    // variant seat with its OWN key: exact wins
		{"jira_prod_eu", "jira_prod"}, // account seat of the variant: longest key wins
		{"jira_staging", "jira"},      // variant seat with a BASE-only key (#1272)
		{"srv_clienta", "srv"},        // named-account seat of srv
		{"srvette", ""},               // shared prefix without the underscore boundary
		{"declared", ""},              // declared, not Optional
		{"declared_clienta", ""},      // ...nor are its variant seats
		{"unrelated", ""},             // no key governs it
	}
	for _, tc := range cases {
		// Loop: Go randomizes map range order on every pass, so a regression to
		// first-match (rather than longest-match) fails within a few iterations.
		for i := 0; i < 64; i++ {
			if got := optionalServerFor(tc.registered, optional); got != tc.want {
				t.Fatalf("optionalServerFor(%q) = %q, want %q", tc.registered, got, tc.want)
			}
			if got := OptionalServerFor(tc.registered, optional); got != tc.want {
				t.Fatalf("OptionalServerFor(%q) = %q, want %q", tc.registered, got, tc.want)
			}
		}
	}
}

// TestOptionalServerForToolNameMatchesRegisteredName is the cross-layer
// agreement check inside agentcore: for every case, the key that governs the
// prompt roster's `mcp_<server>_<tool>` name must be the key that governs
// Gate-1's registered server name. If these two ever disagree, a tool is
// registered-but-hidden (or advertised-but-absent) — the #1272 bug.
func TestOptionalServerForToolNameMatchesRegisteredName(t *testing.T) {
	optional := MCPOptionalSet{"jira": true, "jira_prod": true}
	for _, registered := range []string{"jira", "jira_prod", "jira_staging", "unrelated"} {
		for _, tool := range []string{"search", "create_issue"} {
			gate1 := OptionalServerFor(registered, optional)
			roster := OptionalServerForToolName("mcp_"+registered+"_"+tool, optional)
			if gate1 != roster {
				t.Fatalf("server %q tool %q: Gate-1 key %q != roster key %q", registered, tool, gate1, roster)
			}
		}
	}
	// The whole-name branch must stay OFF for roster names: `mcp_jira_search`
	// is server "jira" tool "search", never a server literally named
	// "jira_search" (whose own roster names all carry a further _<tool>).
	both := MCPOptionalSet{"jira": true, "jira_search": true}
	if got := OptionalServerForToolName("mcp_jira_search", both); got != "jira" {
		t.Fatalf("OptionalServerForToolName(mcp_jira_search) = %q, want jira", got)
	}
	if got := OptionalServerForToolName("mcp_jira_search_issues", both); got != "jira_search" {
		t.Fatalf("OptionalServerForToolName(mcp_jira_search_issues) = %q, want jira_search", got)
	}
	// A name without the mcp_ prefix is not a roster name.
	if got := OptionalServerForToolName("jira_search", both); got != "" {
		t.Fatalf("OptionalServerForToolName(jira_search) = %q, want \"\"", got)
	}
}

// TestGate1GatesVariantSeatUnderBaseOptionalKey is the #1272 regression: a
// named-account seat registers as "<server>_<account>", so an Optional spec
// keyed only on the BASE server name ("jira") used to miss it on Gate-1's exact
// map lookup — the seat's tools registered and were CALLABLE on every run,
// while the system-prompt roster (which resolves the base key by prefix) hid
// them from the model. Gate-1 now resolves the same key, so the variant is
// gated: nothing registers until the base server is opted in.
func TestGate1GatesVariantSeatUnderBaseOptionalKey(t *testing.T) {
	catalog := []mcp.ServerTool{
		variantTool("jira", "search"),
		variantTool("jira_prod", "search"),
		variantTool("public", "ping"),
	}
	optional := mcpOptionalSet{"jira": true}

	// Not opted in: the default seat AND the variant seat are both withheld.
	// The non-Optional server is unaffected.
	registered, err := buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, optional, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	names := toolNamesOf(registered)
	for _, blocked := range []string{"mcp_jira_search", "mcp_jira_prod_search"} {
		if names[blocked] {
			t.Errorf("%s registered without an opt-in: the Optional gate did not govern the seat", blocked)
		}
	}
	if !names["mcp_public_ping"] {
		t.Errorf("non-Optional server was gated: %v", names)
	}

	// Opted in by the base key — the key the interactive/scheduled producers
	// actually put in optIn for a {server: jira, account: prod} selection.
	registered, err = buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, optional, map[string]bool{"jira": true}, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	names = toolNamesOf(registered)
	for _, want := range []string{"mcp_jira_search", "mcp_jira_prod_search", "mcp_public_ping"} {
		if !names[want] {
			t.Errorf("opted-in tool %s was not registered: %v", want, names)
		}
	}
}

// TestGate1VariantSeatWithOwnOptionalKey covers the other two cases of the
// acceptance matrix: a variant seat that declares its OWN Optional key is gated
// by that key, and when BOTH the base and the variant are Optional each toggle
// governs exactly its own seat (the longest-key overlap the roster has resolved
// this way since #1206).
func TestGate1VariantSeatWithOwnOptionalKey(t *testing.T) {
	catalog := []mcp.ServerTool{
		variantTool("jira", "search"),
		variantTool("jira_prod", "search"),
	}

	// Variant-only key: the variant is gated by "jira_prod"; the base server is
	// not Optional at all and always registers.
	registered, err := buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, mcpOptionalSet{"jira_prod": true}, nil, toolBuildConfig{})
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	names := toolNamesOf(registered)
	if !names["mcp_jira_search"] || names["mcp_jira_prod_search"] {
		t.Errorf("variant-only Optional key mis-gated: %v", names)
	}

	// Both Optional: one toggle each, in both directions.
	both := mcpOptionalSet{"jira": true, "jira_prod": true}
	for _, tc := range []struct {
		optIn string
		want  string
		other string
	}{
		{optIn: "jira", want: "mcp_jira_search", other: "mcp_jira_prod_search"},
		{optIn: "jira_prod", want: "mcp_jira_prod_search", other: "mcp_jira_search"},
	} {
		registered, err := buildFantasyTools(nil, catalog, &fakeBroker{}, nil, passPolicy{}, both, map[string]bool{tc.optIn: true}, toolBuildConfig{})
		if err != nil {
			t.Fatalf("buildFantasyTools: %v", err)
		}
		names := toolNamesOf(registered)
		if !names[tc.want] || names[tc.other] {
			t.Errorf("optIn %q: want only %s registered, got %v", tc.optIn, tc.want, names)
		}
	}
}
