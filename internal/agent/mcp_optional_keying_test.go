package agent

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestRosterAndGate1AgreeOnOptionalKeying is the cross-layer half of #1272: the
// key the system-prompt roster uses to decide what the model SEES must be the
// key agentcore's Gate-1 uses to decide what actually REGISTERS. Both now
// resolve through one helper, so this test is the fence that keeps a future
// change from re-forking the rule.
//
// The three acceptance cases, over one Optional set per case:
//   - variant seat with its OWN Optional key,
//   - variant seat with a BASE-only key (the case that used to disagree: Gate-1's
//     exact lookup on "jira_prod" missed, so the seat was callable while the
//     roster hid it),
//   - both base and variant Optional (the overlap #1206 aligned).
func TestRosterAndGate1AgreeOnOptionalKeying(t *testing.T) {
	cases := []struct {
		name     string
		optional mcpOptionalSet
	}{
		{"variant has its own key", mcpOptionalSet{"jira_prod": true}},
		{"base-only key", mcpOptionalSet{"jira": true}},
		{"both optional", mcpOptionalSet{"jira": true, "jira_prod": true}},
	}
	registeredSeats := []string{"jira", "jira_prod", "jira_prod_eu", "unrelated"}
	tools := []string{"search", "create_issue"}

	for _, tc := range cases {
		for _, seat := range registeredSeats {
			for _, tool := range tools {
				// Map-order randomization: repeat so a non-deterministic
				// resolution shows up rather than passing by luck.
				for i := 0; i < 32; i++ {
					gate1 := agentcore.OptionalServerFor(seat, agentcore.MCPOptionalSet(tc.optional))
					roster := longestOptionalServerFor("mcp_"+seat+"_"+tool, tc.optional)
					if gate1 != roster {
						t.Fatalf("%s: seat %q tool %q: Gate-1 key %q != roster key %q",
							tc.name, seat, tool, gate1, roster)
					}
				}
			}
		}
	}
}

// TestActiveMCPToolNamesVariantSeatUnderBaseOptionalKey pins the roster side of
// the base-only-key case: a variant seat's tools follow the BASE server's
// toggle, which is exactly what Gate-1 now enforces at registration
// (TestGate1GatesVariantSeatUnderBaseOptionalKey in internal/agentcore).
func TestActiveMCPToolNamesVariantSeatUnderBaseOptionalKey(t *testing.T) {
	m := &Manager{
		mcpToolRoster: []string{
			"mcp_jira_prod_search",
			"mcp_jira_search",
			"mcp_public_ping",
		},
		optionalServers: mcpOptionalSet{"jira": true},
	}

	got := m.activeMCPToolNames(nil)
	if len(got) != 1 || got[0] != "mcp_public_ping" {
		t.Fatalf("activeMCPToolNames(nil) = %v, want only [mcp_public_ping]", got)
	}

	got = m.activeMCPToolNames([]string{"jira"})
	want := []string{"mcp_jira_prod_search", "mcp_jira_search", "mcp_public_ping"}
	if len(got) != len(want) {
		t.Fatalf("activeMCPToolNames([jira]) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activeMCPToolNames([jira]) = %v, want %v", got, want)
		}
	}
}
