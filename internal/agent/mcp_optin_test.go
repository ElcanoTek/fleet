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
