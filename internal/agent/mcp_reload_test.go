package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// mcpHTTPStub starts an in-process MCP-over-HTTP server advertising one tool.
func mcpHTTPStub(t *testing.T, toolName string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []mcp.Tool{{Name: toolName, Description: toolName}}}
		default:
			resp["result"] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestSpecsToServerDefs(t *testing.T) {
	defs := specsToServerDefs(map[string]MCPServerSpec{
		"http":     {Enabled: true, URL: "https://x.test/mcp", Headers: map[string]string{"A": "b"}},
		"stdio":    {Enabled: true, Command: "python", Args: []string{"s.py"}, Dir: "/bundle"},
		"disabled": {Enabled: false, URL: "https://y.test/mcp"},
	})
	byName := map[string]mcp.ServerDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if _, ok := byName["disabled"]; ok {
		t.Error("disabled spec must be dropped")
	}
	if d := byName["http"]; d.URL != "https://x.test/mcp" || d.Headers["A"] != "b" {
		t.Errorf("http def wrong: %+v", d)
	}
	if d := byName["stdio"]; d.Command != "python" || d.Dir != "/bundle" || len(d.Args) != 1 {
		t.Errorf("stdio def wrong: %+v", d)
	}
}

// TestReloadMCPServers_RefreshesGating proves a reload adds a server AND refreshes
// the spec-derived gating — critically, that a newly-added OPTIONAL server is
// registered in optionalServers so it is gated (not treated as always-on, which
// would re-trigger the #433 tool-ceiling overflow).
func TestReloadMCPServers_RefreshesGating(t *testing.T) {
	ctx := context.Background()
	srvA := mcpHTTPStub(t, "tool_a")
	srvB := mcpHTTPStub(t, "tool_b")

	specsA := map[string]MCPServerSpec{
		"A": {Enabled: true, URL: srvA.URL},
	}
	m := &Manager{mcpClient: BuildMCPClient(specsA, nil)}
	t.Cleanup(func() { _ = m.mcpClient.Close() })
	// Seed initial gating the way New() does.
	m.mcpToolRoster = m.computeMCPToolRoster(mcpAllowlist{})
	m.optionalServerMetadata = m.buildOptionalServerMetadata(specsA)

	// Reload: keep A, add B as an OPTIONAL server.
	specsAB := map[string]MCPServerSpec{
		"A": {Enabled: true, URL: srvA.URL},
		"B": {Enabled: true, URL: srvB.URL, Optional: true, Description: "the B server"},
	}
	sum, err := m.ReloadMCPServers(ctx, specsAB)
	if err != nil {
		t.Fatalf("ReloadMCPServers: %v", err)
	}
	if len(sum.Added) != 1 || sum.Added[0] != "B" {
		t.Errorf("summary Added=%v want [B]", sum.Added)
	}

	// Client now serves B's tools.
	var haveB bool
	for _, st := range m.mcpClient.GetAllTools() {
		if st.ServerName == "B" && st.Tool.Name == "tool_b" {
			haveB = true
		}
	}
	if !haveB {
		t.Error("reloaded client should advertise B's tool")
	}

	// #433-critical: B must be recorded as optional (gated), not always-on.
	_, optional := m.mcpGates()
	if !optional["B"] {
		t.Error("newly-added optional server B must be in optionalServers after reload")
	}

	// The picker catalog reflects B.
	var catHasB bool
	for _, info := range m.MCPServerCatalog() {
		if info.Name == "B" {
			catHasB = true
		}
	}
	if !catHasB {
		t.Error("MCPServerCatalog should include the newly-added optional server B")
	}

	// Remove B on a subsequent reload.
	sum, err = m.ReloadMCPServers(ctx, specsA)
	if err != nil {
		t.Fatalf("ReloadMCPServers remove: %v", err)
	}
	if len(sum.Removed) != 1 || sum.Removed[0] != "B" {
		t.Errorf("summary Removed=%v want [B]", sum.Removed)
	}
	if _, optional := m.mcpGates(); optional["B"] {
		t.Error("B should no longer be optional after removal")
	}
}
