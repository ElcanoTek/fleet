package agentcore

import (
	"context"
	"reflect"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// metaRecordingBroker records the fleet `_meta` entries each call carried.
type metaRecordingBroker struct {
	metas []map[string]any
}

func (b *metaRecordingBroker) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	b.metas = append(b.metas, mcp.RequestMeta(ctx))
	return "ok", false, nil
}

// Deps.MCPRequestMeta reaches every MCP call of the run, email or not: a
// private task's deal sheet tool sends its email from inside its own call.
func TestMCPTool_RequestMetaOnEveryCall(t *testing.T) {
	private := map[string]any{mcp.PrivateEmailsMetaKey: true}
	for _, name := range []string{"send_email", "finish_plan_booking"} {
		b := &metaRecordingBroker{}
		tool := &mcpTool{serverName: "ses_outbound", tool: mcp.Tool{Name: name, Description: "d"}, broker: b, requestMeta: private}
		if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: `{"to_email":"x@example.com"}`}); err != nil {
			t.Fatal(err)
		}
		if len(b.metas) != 1 || !reflect.DeepEqual(b.metas[0], private) {
			t.Fatalf("%s: meta = %v, want %v", name, b.metas, private)
		}
	}

	// Without it (every chat turn, a task not set to send privately) the call
	// carries no fleet `_meta`.
	b := &metaRecordingBroker{}
	tool := &mcpTool{serverName: "ses_outbound", tool: mcp.Tool{Name: "send_email", Description: "d"}, broker: b}
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc", Input: `{}`}); err != nil {
		t.Fatal(err)
	}
	if len(b.metas) != 1 || b.metas[0] != nil {
		t.Fatalf("no request meta: got %v", b.metas)
	}
}
