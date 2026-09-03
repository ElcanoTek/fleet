package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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

func TestReloadMCPServers_InjectedBrokerRefreshesPublicState(t *testing.T) {
	oldCatalog := []mcp.ServerTool{{ServerName: "A", Tool: mcp.Tool{Name: "tool_a"}}}
	newCatalog := []mcp.ServerTool{{ServerName: "B", Tool: mcp.Tool{Name: "tool_b"}}}
	var called bool
	m := &Manager{
		mcpBroker:         inertMCPBroker{},
		mcpCatalog:        oldCatalog,
		mcpAccounts:       map[string][]string{"A": {"old"}},
		allowlist:         mcpAllowlist{"A": {"tool_a"}},
		optionalServers:   mcpOptionalSet{"A": true},
		enabledMCPServers: map[string]bool{"A": true},
		mcpToolRoster:     []string{"mcp_A_tool_a"},
	}
	m.reloadMCP = func(context.Context) (*MCPReloadResult, error) {
		called = true
		_, optional := m.mcpGates()
		if !optional["A"] || optional["B"] {
			t.Error("broker reload changed gates before its self-describing result")
		}
		return &MCPReloadResult{
			Summary:  mcp.ReloadSummary{Added: []string{"B"}, Removed: []string{"A"}},
			Catalog:  newCatalog,
			Accounts: map[string][]string{"B": {"broker-seat"}},
			Specs: map[string]MCPServerSpec{
				"B": {
					Enabled:       true,
					Optional:      true,
					Description:   "the B server",
					ToolAllowlist: []string{"tool_b"},
					AccountVars:   []string{"B_TOKEN"},
				},
			},
		}, nil
	}
	m.optionalServerMetadata = m.buildOptionalServerMetadata(map[string]MCPServerSpec{
		"A": {Enabled: true, Optional: true},
	})

	ignoredParentSpecs := map[string]MCPServerSpec{
		"B": {
			Enabled:  true,
			Optional: false,
		},
	}
	summary, err := m.ReloadMCPServers(context.Background(), ignoredParentSpecs)
	if err != nil {
		t.Fatalf("ReloadMCPServers: %v", err)
	}
	if !called || len(summary.Added) != 1 || summary.Added[0] != "B" {
		t.Fatalf("called=%v summary=%+v", called, summary)
	}
	if !m.MCPReloadOwnsConfig() {
		t.Fatal("injected reload seam should own connector configuration")
	}
	if got := m.MCPCatalog(); len(got) != 1 || got[0].ServerName != "B" || got[0].Tool.Name != "tool_b" {
		t.Fatalf("catalog = %+v, want B.tool_b", got)
	}
	_, gotRoster := m.mcpRosterSnapshot()
	if got := gotRoster; len(got) != 1 || got[0] != "mcp_B_tool_b" {
		t.Fatalf("roster = %v, want mcp_B_tool_b", got)
	}
	picker := m.MCPServerCatalog()
	if len(picker) < 1 || picker[0].Name != "B" || len(picker[0].Accounts) != 1 || picker[0].Accounts[0] != "broker-seat" {
		t.Fatalf("picker = %+v, want B with broker-seat", picker)
	}
}

func TestReloadMCPServers_InjectedBrokerFailureRevertsGates(t *testing.T) {
	wantErr := errors.New("broker reload failed")
	m := &Manager{
		mcpBroker:         inertMCPBroker{},
		mcpCatalog:        []mcp.ServerTool{{ServerName: "A", Tool: mcp.Tool{Name: "tool_a"}}},
		allowlist:         mcpAllowlist{"A": {"tool_a"}},
		optionalServers:   mcpOptionalSet{"A": true},
		enabledMCPServers: map[string]bool{"A": true},
		reloadMCP: func(context.Context) (*MCPReloadResult, error) {
			return nil, wantErr
		},
	}
	_, err := m.ReloadMCPServers(context.Background(), map[string]MCPServerSpec{
		"B": {Enabled: true, Optional: true},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ReloadMCPServers = %v, want %v", err, wantErr)
	}
	allow, optional := m.mcpGates()
	if !optional["A"] || optional["B"] || len(allow["A"]) != 1 || !m.enabledMCPServers["A"] || m.enabledMCPServers["B"] {
		t.Fatalf("gates not reverted: allow=%v optional=%v enabled=%v", allow, optional, m.enabledMCPServers)
	}
	if got := m.MCPCatalog(); len(got) != 1 || got[0].ServerName != "A" {
		t.Fatalf("catalog changed on failed reload: %+v", got)
	}
}

func TestReloadMCPServers_InjectedBrokerWithoutReloadSeamFails(t *testing.T) {
	m := &Manager{mcpBroker: inertMCPBroker{}}
	_, err := m.ReloadMCPServers(context.Background(), nil)
	if err == nil || err.Error() != "MCP reload unavailable for injected broker" {
		t.Fatalf("ReloadMCPServers = %v, want explicit unavailable error", err)
	}
}

func TestSpecsToServerDefs(t *testing.T) {
	defs := MCPServerDefs(map[string]MCPServerSpec{
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
	// Dir is no longer the bundle root: a stdio server launches in the shared
	// MCP workspace so its relative output paths stop landing in the operator's
	// bundle checkout (agentcore.StdioCwd). What the reload path MUST preserve
	// is that this matches BuildMCPClient's spawn exactly — serverDefEqual
	// compares Dir, so a mismatch here restarts every server on every reload.
	if d := byName["stdio"]; d.Command != "python" || len(d.Args) != 1 {
		t.Errorf("stdio def wrong: %+v", d)
	}
	wantDir := agentcore.StdioCwd("/bundle", false, agentcore.SharedMCPWorkspaceDir())
	if d := byName["stdio"]; d.Dir != wantDir {
		t.Errorf("stdio Dir = %q, want %q (must equal the boot spawn's cwd or every reload churns)", d.Dir, wantDir)
	}
}

// TestSpecsToServerDefs_PinnedDirSurvives — an Agent Plugin server must still
// launch in its plugin root. Its args are opaque strings fleet may not rewrite
// and it resolves bundled files against that root (ADR-0054), so a workspace
// must never displace it.
func TestSpecsToServerDefs_PinnedDirSurvives(t *testing.T) {
	dir := t.TempDir() // exists, so it would be chosen if the pin were ignored
	defs := MCPServerDefs(map[string]MCPServerSpec{
		"plugin": {Enabled: true, Command: "python", Args: []string{"s.py"}, Dir: dir, DirPinned: true},
	})
	if len(defs) != 1 {
		t.Fatalf("want 1 def, got %d", len(defs))
	}
	if defs[0].Dir != dir {
		t.Errorf("pinned Dir = %q, want the plugin root %q", defs[0].Dir, dir)
	}
}

// TestReloadMCPServers_RefreshesGating proves a reload adds a server AND refreshes
// the spec-derived gating — critically, that a newly-added OPTIONAL server is
// registered in optionalServers so it is gated (not treated as always-on, which
// would re-trigger the #433 tool-ceiling overflow).
func TestReloadMCPServers_RefreshesGating(t *testing.T) {
	ctx := context.Background()
	t.Setenv("B_TOKEN_BLUE", "test-placeholder")
	srvA := mcpHTTPStub(t, "tool_a")
	srvB := mcpHTTPStub(t, "tool_b")

	specsA := map[string]MCPServerSpec{
		"A": {Enabled: true, URL: srvA.URL},
	}
	m := &Manager{mcpClient: BuildMCPClient(specsA, nil, nil)}
	t.Cleanup(func() { _ = m.mcpClient.Close() })
	// Seed initial gating the way New() does.
	m.mcpToolRoster = m.computeMCPToolRoster(mcpAllowlist{})
	m.optionalServerMetadata = m.buildOptionalServerMetadata(specsA)

	// Reload: keep A, add B as an OPTIONAL server.
	specsAB := map[string]MCPServerSpec{
		"A": {Enabled: true, URL: srvA.URL},
		"B": {
			Enabled:     true,
			URL:         srvB.URL,
			Optional:    true,
			Description: "the B server",
			AccountVars: []string{"B_TOKEN"},
		},
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
			if len(info.Accounts) != 1 || info.Accounts[0] != "blue" {
				t.Errorf("reloaded B accounts = %v, want [blue]", info.Accounts)
			}
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
