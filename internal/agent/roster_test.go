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

// TestActiveMCPToolNamesOverlappingOptionalServersLongestPrefix pins the
// prompt-cache byte-stability fix (#1125): with OVERLAPPING Optional server
// names — real under the `<server>_<account>` variant convention ("jira",
// "jira_prod") — the roster filter must attribute each `mcp_<server>_<tool>`
// name to the LONGEST matching server, never to whichever overlapping key a
// map range happened to visit first. The old first-match break made a
// variant's tool appear in (or vanish from) the system prompt per-turn at
// random, silently busting the cacheable prefix
// (docs/PROMPT-CACHE-CONTRACT.md). The loop count flushes out map-order
// dependence: Go randomizes range order on every iteration, so a regression
// to first-match fails within a few passes.
func TestActiveMCPToolNamesOverlappingOptionalServersLongestPrefix(t *testing.T) {
	m := &Manager{
		mcpToolRoster: []string{
			"mcp_jira_search",
			"mcp_jira_prod_search",
		},
		optionalServers: mcpOptionalSet{
			"jira":      true,
			"jira_prod": true,
		},
	}
	// Only the SHORT name is opted in: mcp_jira_prod_search belongs to
	// "jira_prod" (the longest prefix), which is NOT enabled, so it must be
	// filtered on every evaluation; mcp_jira_search matches only "jira" and
	// stays.
	for i := 0; i < 200; i++ {
		got := m.activeMCPToolNames([]string{"jira"})
		if len(got) != 1 || got[0] != "mcp_jira_search" {
			t.Fatalf("iteration %d: activeMCPToolNames = %v, want [mcp_jira_search] (longest-prefix, map-order-independent)", i, got)
		}
	}
	// The inverse opt-in keeps only the variant's tool.
	for i := 0; i < 200; i++ {
		got := m.activeMCPToolNames([]string{"jira_prod"})
		if len(got) != 1 || got[0] != "mcp_jira_prod_search" {
			t.Fatalf("iteration %d: activeMCPToolNames = %v, want [mcp_jira_prod_search]", i, got)
		}
	}
}
