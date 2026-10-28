package agent

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The chat system prompt's tool-gated sections read activeMCPToolNames, so a
// tool agent_policy.interactive_hidden_tools keeps out of chat must not count
// there: the model would be pointed at guidance for tools it cannot call
// (docs/INTERACTIVE-HIDDEN-TOOLS.md). The list is the turn's snapshot, the
// same slice the run gate gets.
func TestActiveMCPToolNames_SubtractsInteractiveHiddenTools(t *testing.T) {
	m := &Manager{
		mcpToolRoster: []string{"mcp_ox_ox_create_deal", "mcp_ox_ox_preview_plan", "mcp_xandr_create_deal"},
		optionalServers: mcpOptionalSet{
			"xandr": true,
		},
	}
	if got, want := m.activeMCPToolNames([]string{"xandr"}, []string{"create_deal"}), []string{"mcp_ox_ox_preview_plan"}; !slices.Equal(got, want) {
		t.Fatalf("hidden list set: activeMCPToolNames = %v, want %v", got, want)
	}
	// Without a list the roster is what it was before.
	if got := m.activeMCPToolNames([]string{"xandr"}, nil); len(got) != 3 {
		t.Fatalf("no list: activeMCPToolNames = %v, want all 3", got)
	}
}

// The fast.io protocol entry follows what chat can call: hidden fast.io tools
// do not list it, and one visible fast.io tool still does.
func TestBuildSystemPrompt_FastIOGatedOnVisibleTools(t *testing.T) {
	m := fixtureManager(t)
	writeFile(t, filepath.Join(m.protocolsDir, "fastio-mcp.md"), "# Fast.io Protocol\n")
	m.mcpToolRoster = []string{"mcp_fast_io_storage", "mcp_fast_io_workspace"}

	for _, tc := range []struct {
		name   string
		hidden []string
		listed bool
	}{
		{"every fast.io tool hidden", []string{"fast_io_storage", "fast_io_workspace"}, false},
		{"one fast.io tool still visible", []string{"fast_io_storage"}, true},
		{"nothing hidden", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, err := m.buildSystemPrompt("victoria", nil, "conv-x", nil, "", nil, nil, nil, hostedMCPRoster{}, tc.hidden)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(prompt, "protocols/fastio-mcp.md"); got != tc.listed {
				t.Fatalf("fastio-mcp.md listed = %t, want %t", got, tc.listed)
			}
		})
	}
}
