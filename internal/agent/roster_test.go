package agent

import (
	"testing"
)

// TestActiveMCPToolNamesReturnsSnapshot pins the cache-friendliness contract:
// the system-prompt roster comes from the frozen slice captured at
// Manager.New(), not a live walk of mcpClient.GetAllTools(). A nil mcpClient
// + a populated roster should still return the roster — that's exactly the
// mid-conversation-disconnect case we care about. If this test ever fails
// the server has regressed to live reads, which silently busts Anthropic
// cache_control breakpoints and OpenAI implicit-cache prefixes.
func TestActiveMCPToolNamesReturnsSnapshot(t *testing.T) {
	m := &Manager{
		mcpClient:     nil,
		mcpToolRoster: []string{"mcp_sendgrid_send_email", "mcp_tavily_search"},
	}
	got := m.activeMCPToolNames(nil)
	if len(got) != 2 || got[0] != "mcp_sendgrid_send_email" || got[1] != "mcp_tavily_search" {
		t.Fatalf("activeMCPToolNames() = %v, want the frozen snapshot", got)
	}
}

func TestActiveMCPToolNamesFiltersDisabledOptionalServers(t *testing.T) {
	m := &Manager{
		mcpToolRoster: []string{
			"mcp_email_search_emails",
			"mcp_fast_io_storage",
			"mcp_xandr_xandr_auth_status",
			"mcp_indexexchange_ix_auth_status",
		},
		optionalServers: mcpOptionalSet{
			"xandr":         true,
			"indexexchange": true,
		},
	}

	got := m.activeMCPToolNames([]string{"xandr"})
	want := []string{"mcp_email_search_emails", "mcp_fast_io_storage", "mcp_xandr_xandr_auth_status"}
	if len(got) != len(want) {
		t.Fatalf("activeMCPToolNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activeMCPToolNames() = %v, want %v", got, want)
		}
	}
}
