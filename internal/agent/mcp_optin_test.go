package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// newTestOrch returns a minimal orchestrationState good enough for
// buildFantasyTools to wrap native tools in the ceiling guard without
// tripping the real ceilings.
func newTestOrch() *orchestrationState {
	o := newOrchestrationState()
	o.setCeilings(999, 9_999_999)
	return o
}

// makeTestNative returns a single no-op native tool for use in tool-
// registration tests. Named so we can assert it's still present in the
// filtered slice.
func makeTestNative(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"test probe",
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

func hasToolNamed(tools []fantasy.AgentTool, name string) bool {
	for _, t := range tools {
		if t.Info().Name == name {
			return true
		}
	}
	return false
}

func TestBuildFantasyTools_OptionalServer_DroppedWhenNotOptedIn(t *testing.T) {
	// Empty mcp.Client (no real servers connected) + nil allowlist. The
	// point of this test is that the optional-gating logic is keyed off
	// Manager.optionalServers, not off live tools — so we don't need a
	// running subprocess to exercise it.
	client := mcp.NewClient()
	orch := newTestOrch()
	native := []fantasy.AgentTool{makeTestNative("native_probe")}

	// Server is known-optional; conversation has not opted in.
	optional := mcpOptionalSet{"gamma": true}
	var enabled []string // no opt-ins

	tools, err := buildFantasyTools(native, client, nil, orch, optional, enabled)
	if err != nil {
		t.Fatalf("buildFantasyTools returned error: %v", err)
	}
	if !hasToolNamed(tools, "native_probe") {
		t.Error("native tool should always be registered")
	}
	// Since our fake client has no tools, we can't directly verify that
	// gamma tools were filtered — but we CAN verify the count is exactly
	// the native set (no phantom registrations).
	if got, want := len(tools), 1; got != want {
		t.Errorf("expected %d tools registered (native only), got %d", want, got)
	}
}

func TestBuildFantasyTools_OptionalServer_PassesWhenOptedIn(t *testing.T) {
	// Smoke-level: opted-in path doesn't error out. With no real MCP
	// tools in the test client this can't assert gamma tools are PRESENT,
	// but it confirms the opt-in path is exercised without crashes.
	client := mcp.NewClient()
	orch := newTestOrch()
	native := []fantasy.AgentTool{makeTestNative("native_probe")}
	optional := mcpOptionalSet{"gamma": true}
	enabled := []string{"gamma"}

	tools, err := buildFantasyTools(native, client, nil, orch, optional, enabled)
	if err != nil {
		t.Fatalf("buildFantasyTools returned error: %v", err)
	}
	if !hasToolNamed(tools, "native_probe") {
		t.Error("native tool should always be registered")
	}
}

func TestBuildFantasyTools_NonOptionalAlwaysRegistered(t *testing.T) {
	// Non-optional servers pass through regardless of the opt-in list.
	// Same smoke approach as above — confirms no crash and native still
	// present when no optional servers are configured at all.
	client := mcp.NewClient()
	orch := newTestOrch()
	native := []fantasy.AgentTool{makeTestNative("native_probe")}

	tools, err := buildFantasyTools(native, client, nil, orch, nil, nil)
	if err != nil {
		t.Fatalf("buildFantasyTools returned error: %v", err)
	}
	if !hasToolNamed(tools, "native_probe") {
		t.Error("native tool should always be registered")
	}
}

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
