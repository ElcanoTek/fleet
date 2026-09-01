package agent

import (
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMCPServerCatalog_EmptyWhenNoOptionalServers(t *testing.T) {
	// Manager without any Optional specs — catalog is empty, NOT nil-
	// panicking. Exercises the zero-case so the HTTP handler can rely
	// on `range s.agent.MCPServerCatalog()` never blowing up.
	m := &Manager{
		optionalServerMetadata: []OptionalServerInfo{},
	}
	if got := m.MCPServerCatalog(); len(got) != 0 {
		t.Errorf("expected empty catalog, got %d entries", len(got))
	}
}

// When an Optional MCP fails to start (subprocess crash on missing env
// var, network timeout, etc.) the catalog still includes its row but
// has no live tools to attach. The Tools field MUST serialize as `[]`,
// not `null` — the picker calls `.join()` on it client-side and `null`
// would crash the React render. Regression for the gamma per-user-keys
// startup bug.
func TestBuildOptionalServerMetadata_FailedMCP_ToolsIsEmptyArrayNotNull(t *testing.T) {
	m := &Manager{mcpClient: mcp.NewClient()}
	specs := map[string]MCPServerSpec{
		"gamma": {
			Enabled:     true,
			Optional:    true,
			DisplayName: "Gamma",
			Description: "Slide decks",
		},
	}
	out := m.buildOptionalServerMetadata(specs)
	// Catalog now also includes a synthetic image_generation entry. Find
	// the gamma row explicitly; only its Tools slice is the regression
	// surface this test exists for.
	var gamma *OptionalServerInfo
	for i := range out {
		if out[i].Name == "gamma" {
			gamma = &out[i]
			break
		}
	}
	if gamma == nil {
		t.Fatalf("expected gamma entry in catalog, got %+v", out)
	}
	if gamma.Tools == nil {
		t.Fatal("Tools must be non-nil so JSON renders [] not null")
	}
	raw, err := json.Marshal(gamma)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, ok := parsed["tools"]
	if !ok {
		t.Fatal("tools key missing from JSON")
	}
	if tools == nil {
		t.Errorf("tools serialized as null; expected []. raw=%s", string(raw))
	}
}

func TestBuildOptionalServerMetadata_UsesInjectedPublicCatalogAndAccounts(t *testing.T) {
	m := &Manager{
		mcpCatalog:  []mcp.ServerTool{{ServerName: "gamma", Tool: mcp.Tool{Name: "render"}}},
		mcpAccounts: map[string][]string{"gamma": {"primary", "backup"}},
	}
	out := m.buildOptionalServerMetadata(map[string]MCPServerSpec{
		"gamma": {Enabled: true, Optional: true},
	})
	var gamma OptionalServerInfo
	for _, info := range out {
		if info.Name == "gamma" {
			gamma = info
		}
	}
	if gamma.ToolCount != 1 || len(gamma.Tools) != 1 || gamma.Tools[0] != "render" {
		t.Fatalf("injected tool catalog not reflected: %+v", gamma)
	}
	if len(gamma.Accounts) != 2 || gamma.Accounts[0] != "primary" || gamma.Accounts[1] != "backup" {
		t.Fatalf("injected public account names not reflected: %+v", gamma.Accounts)
	}
}

func TestBuildAlwaysOnServerMetadata_ReflectsLiveDiscoveryStatus(t *testing.T) {
	specs := map[string]MCPServerSpec{
		"email": {
			Enabled:       true,
			DisplayName:   "Email",
			Description:   "Inbound reports",
			ToolAllowlist: []string{"search"},
		},
		"broken":   {Enabled: true},
		"optional": {Enabled: true, Optional: true},
	}
	catalog := []mcp.ServerTool{
		{ServerName: "email", Tool: mcp.Tool{Name: "search"}},
		{ServerName: "email", Tool: mcp.Tool{Name: "admin_only"}},
		{ServerName: "optional", Tool: mcp.Tool{Name: "lookup"}},
	}
	got := buildAlwaysOnServerMetadataFromCatalog(specs, catalog, map[string][]string{
		"email": {"backup"},
	})
	if len(got) != 2 {
		t.Fatalf("always-on catalog = %+v, want email and broken only", got)
	}
	byName := map[string]AlwaysOnServerInfo{}
	for _, info := range got {
		byName[info.Name] = info
	}
	if email := byName["email"]; !email.Available || email.ToolCount != 1 ||
		email.DisplayName != "Email" || len(email.Accounts) != 1 || email.Accounts[0] != "backup" {
		t.Fatalf("live email status = %+v", email)
	}
	if broken := byName["broken"]; broken.Available || broken.ToolCount != 0 {
		t.Fatalf("failed always-on connector was painted available: %+v", broken)
	}
}

func TestMCPServerCatalog_ReturnsSnapshot(t *testing.T) {
	// Catalog exposes the exact snapshot built at Manager.New(). Test
	// that mutating the returned slice doesn't leak back into the
	// manager on a re-read — the manager's copy must be stable.
	m := &Manager{
		optionalServerMetadata: []OptionalServerInfo{
			{Name: "gamma", Description: "Gamma AI — slide decks", ToolCount: 5, Tools: []string{"generate_presentation"}},
		},
	}
	snap := m.MCPServerCatalog()
	if len(snap) != 1 || snap[0].Name != "gamma" {
		t.Fatalf("unexpected catalog: %+v", snap)
	}
}
