package mcpbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// metaBackend records the fleet `_meta` entries its call context carries, the
// way mcp.Server.callTool would read them.
type metaBackend struct {
	*fakeBroker
	meta map[string]any
}

func (b *metaBackend) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	b.meta = mcp.RequestMeta(ctx)
	return "done", false, nil
}

// A call's fleet `_meta` entries cross the real broker protocol and are
// re-attached on the child's call context; a call without any carries none.
func TestClientServer_ForwardsFleetRequestMeta(t *testing.T) {
	backend := &metaBackend{fakeBroker: &fakeBroker{}}
	client := loopback(t, backend)

	if _, _, err := client.CallMCP(context.Background(), "deals", "execute_plan", nil); err != nil {
		t.Fatal(err)
	}
	if backend.meta != nil {
		t.Fatalf("a plain call reached the child with meta %v", backend.meta)
	}

	ctx := mcp.WithRequestMeta(context.Background(), map[string]any{
		"fleet/narrowed": map[string]any{"arg": "deal_ids", "staged": []any{"a", "b"}},
	})
	if _, _, err := client.CallMCP(ctx, "deals", "execute_plan", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(backend.meta)
	if string(got) != `{"fleet/narrowed":{"arg":"deal_ids","staged":["a","b"]}}` {
		t.Fatalf("child meta = %s", got)
	}
}

// The child keeps only fleet/ keys of what the parent sends, so a parent
// cannot set any other `_meta` key (a progress token, say) through the wire.
func TestServer_DropsNonFleetRequestMeta(t *testing.T) {
	backend := &metaBackend{fakeBroker: &fakeBroker{}}
	client := loopback(t, backend)
	resp, err := client.roundtrip(context.Background(), request{
		ID: client.nextID.Add(1), Method: methodCall, Server: "deals", Tool: "execute_plan",
		Meta: map[string]any{"progressToken": "forged", "fleet/x": true},
	})
	if err != nil || resp.Err != "" {
		t.Fatalf("roundtrip = %+v, %v", resp, err)
	}
	if len(backend.meta) != 1 || backend.meta["fleet/x"] != true {
		t.Fatalf("child meta = %v, want only fleet/x", backend.meta)
	}
}
