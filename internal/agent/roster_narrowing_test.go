package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// driverDeniesAll stands in for the scheduled driver's never-matching deny
// entry (scheduledrun rosterNarrowingDeniesAll).
const driverDeniesAll = "__roster_narrowing_denies_all_tools__"

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
		// A seat with no entry of its own (one loaded mid-run has none).
		{ServerName: "pages_acct2", Tool: mcp.Tool{Name: "get_page_data", InputSchema: map[string]any{"type": "object"}}},
	}
	// The driver's shape: the required server's entry, an explicit deny for
	// every other server it saw at dispatch.
	a.mcpToolAllowlist = agentcore.MCPAllowlist{"pages": {"get_page_data"}, "fast_io": {driverDeniesAll}}
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
