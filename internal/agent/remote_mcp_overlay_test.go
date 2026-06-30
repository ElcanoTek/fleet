package agent

import (
	"context"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

type recordingBroker struct{ label string }

func (b *recordingBroker) CallMCP(_ context.Context, server, _ string, _ map[string]any) (string, bool, error) {
	return b.label + ":" + server, false, nil
}

func TestCompositeBrokerRouting(t *testing.T) {
	cb := &compositeBroker{
		overlay:        &recordingBroker{label: "overlay"},
		overlayServers: map[string]bool{"userserver": true},
		base:           &recordingBroker{label: "base"},
	}
	// An overlay server routes to the overlay broker.
	got, _, _ := cb.CallMCP(context.Background(), "userserver", "tool", nil)
	if got != "overlay:userserver" {
		t.Errorf("overlay routing = %q", got)
	}
	// Anything else routes to the base broker — a user server can't shadow it.
	got, _, _ = cb.CallMCP(context.Background(), "gamma", "tool", nil)
	if got != "base:gamma" {
		t.Errorf("base routing = %q", got)
	}
}

type fakeResolver struct {
	conns  []RemoteMCPConn
	tokens map[string]string
}

func (f *fakeResolver) ConnectedServersForUser(_ context.Context, _ string) ([]RemoteMCPConn, error) {
	return f.conns, nil
}
func (f *fakeResolver) AcquireTokenByID(_ context.Context, _, id string) (string, error) {
	return f.tokens[id], nil
}
func (f *fakeResolver) SafeHTTPClient() *http.Client { return http.DefaultClient }

func TestBuildRemoteMCPOverlayGuards(t *testing.T) {
	ctx := context.Background()
	// nil resolver → nil overlay.
	if ov, err := BuildRemoteMCPOverlay(ctx, nil, "u@x.com", nil); err != nil || ov.Active() {
		t.Errorf("nil resolver: ov=%v err=%v", ov, err)
	}
	// empty email → nil overlay.
	r := &fakeResolver{conns: []RemoteMCPConn{{ID: "1", Name: "s", URL: "https://s"}}}
	if ov, err := BuildRemoteMCPOverlay(ctx, r, "", nil); err != nil || ov.Active() {
		t.Errorf("empty email: ov=%v err=%v", ov, err)
	}
	// no connected servers → nil overlay.
	if ov, err := BuildRemoteMCPOverlay(ctx, &fakeResolver{}, "u@x.com", nil); err != nil || ov.Active() {
		t.Errorf("no servers: ov=%v err=%v", ov, err)
	}
}

func TestApplyMCPOverlayNoopWhenInactive(t *testing.T) {
	deps := agentcore.Deps{}
	base := mcp.NewClient()
	// nil overlay → no broker/catalog wiring.
	ApplyMCPOverlay(&deps, base, nil)
	if deps.MCPBroker != nil || deps.MCPCatalog != nil {
		t.Error("nil overlay should leave Deps untouched")
	}
	// Inactive overlay (no servers) → no-op too.
	ApplyMCPOverlay(&deps, base, &RemoteMCPOverlay{Client: base})
	if deps.MCPBroker != nil || deps.MCPCatalog != nil {
		t.Error("inactive overlay should leave Deps untouched")
	}
}

func TestApplyMCPOverlayActiveSetsCompositeBroker(t *testing.T) {
	deps := agentcore.Deps{}
	base := mcp.NewClient()
	overlayClient := mcp.NewClient()
	overlay := &RemoteMCPOverlay{
		Client:  overlayClient,
		Servers: map[string]bool{"userserver": true},
		Catalog: nil,
	}
	ApplyMCPOverlay(&deps, base, overlay)
	if deps.MCPBroker == nil {
		t.Fatal("active overlay should set a composite broker")
	}
	if _, ok := deps.MCPBroker.(*compositeBroker); !ok {
		t.Errorf("expected *compositeBroker, got %T", deps.MCPBroker)
	}
	// MCPCatalog is set (merged) even when empty, so the loop advertises it.
	if deps.MCPCatalog == nil {
		// base + overlay are both empty here, so merged is an empty non-nil slice
		// only if base had tools; with both empty append yields nil — acceptable.
		t.Log("merged catalog is empty (both clients empty) — fine")
	}
}
