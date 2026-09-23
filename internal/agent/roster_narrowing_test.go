package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// A sub-agent of a narrowed run (#1603) can never see more MCP tools than its
// parent: the exhaustive flag carries over, and an explore child's derivation —
// which reads a missing entry as "allow all" — gets an explicit entry for every
// catalog server first.
func TestBuildChild_InheritsTheNarrowedRoster(t *testing.T) {
	catalog := []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "get_page_data"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "deploy_page_upload"}},
		{ServerName: "pages_acct", Tool: mcp.Tool{Name: "get_page_data"}},
		{ServerName: "fast_io", Tool: mcp.Tool{Name: "list_files"}},
	}
	parent := NewAgent(Options{
		Config:             &config.Config{},
		MCPCatalog:         catalog,
		MCPToolAllowlist:   agentcore.MCPAllowlist{"pages": {"get_page_data"}},
		MCPRosterNarrowing: "required_tools_only",
		SystemPrompt:       "parent",
	})
	for _, role := range []string{SubagentRoleWorker, SubagentRoleExplore} {
		child := parent.buildChild(role, nil, nil, nil, 0, 0, 0)
		if child.mcpRosterNarrowing != "required_tools_only" {
			t.Fatalf("%s child lost the exhaustive Gate-2", role)
		}
		allow := child.mcpToolAllowlist
		if got := allow["fast_io"]; len(got) != 1 || got[0] != exploreNoToolsSentinel {
			t.Fatalf("%s child: fast_io = %v, want the deny-all sentinel (the parent registers none of it)", role, got)
		}
		if got := allow["pages"]; !slices.Equal(got, []string{"get_page_data"}) {
			t.Fatalf("%s child: pages = %v, want the parent's [get_page_data]", role, got)
		}
		if got := allow["pages_acct"]; !slices.Equal(got, []string{"get_page_data"}) {
			t.Fatalf("%s child: the seat = %v, want its base server's narrowed entry", role, got)
		}
	}

	// An un-narrowed parent's child is derived exactly as before.
	open := NewAgent(Options{Config: &config.Config{}, MCPCatalog: catalog, SystemPrompt: "parent"})
	if child := open.buildChild(SubagentRoleWorker, nil, nil, nil, 0, 0, 0); child.mcpRosterNarrowing != "" || child.mcpToolAllowlist != nil {
		t.Fatalf("an un-narrowed parent's worker child changed: narrowing=%q allowlist=%v", child.mcpRosterNarrowing, child.mcpToolAllowlist)
	}
}

// End to end through the scheduled driver: the tool list the model is sent is
// the narrowed MCP roster plus the native set (confirm_audit, task_tracker, …),
// and the run log carries the one [roster] breadcrumb.
func TestScheduledRunSendsOnlyTheNarrowedMCPTools(t *testing.T) {
	var sent []string
	model := &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		if sent == nil {
			for _, tool := range call.Tools {
				sent = append(sent, tool.GetName())
			}
		}
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.mcpCatalog = []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "get_page_data", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "deploy_page_upload", InputSchema: map[string]any{"type": "object"}}},
		{ServerName: "fast_io", Tool: mcp.Tool{Name: "share", InputSchema: map[string]any{"type": "object"}}},
	}
	a.mcpToolAllowlist = agentcore.MCPAllowlist{"pages": {"get_page_data"}}
	a.mcpRosterNarrowing = "required_tools_only"
	_ = a.Execute(context.Background(), "Refresh the page data.")

	var mcpSent []string
	for _, name := range sent {
		if strings.HasPrefix(name, "mcp_") && name != "mcp_list_servers" && name != "mcp_load_servers" {
			mcpSent = append(mcpSent, name)
		}
	}
	if !slices.Equal(mcpSent, []string{"mcp_pages_get_page_data"}) {
		t.Fatalf("MCP tools sent to the model = %v, want only mcp_pages_get_page_data", mcpSent)
	}
	if !slices.Contains(sent, "confirm_audit") {
		t.Fatalf("native/control tools must survive the narrowing, sent %v", sent)
	}
	crumbs := 0
	for _, m := range a.logSession.SnapshotMessages() {
		if strings.HasPrefix(m.Content, "[roster] required_tools_only: 1 mcp tools registered") {
			crumbs++
		}
	}
	if crumbs != 1 {
		t.Fatalf("want one [roster] breadcrumb, got %d", crumbs)
	}
}
