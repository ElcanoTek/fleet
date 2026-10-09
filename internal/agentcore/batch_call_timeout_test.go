package agentcore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

func dealIDsInput(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%q", fmt.Sprintf("D%d", i+1))
	}
	return `{"deal_ids":[` + strings.Join(ids, ",") + `],"values_sha256":"abc"}`
}

// TestToolCallTimeoutFor pins the batch timeout scaling ported from cutlass
// toolCallTimeoutForServer (cutlass#1067): deal_ids × per-deal pace, floored at
// toolCallTimeout and capped at maxBatchToolCallTimeout, with the Nexxen pace
// for nexxen_mcp and its client variants.
func TestToolCallTimeoutFor(t *testing.T) {
	cases := []struct {
		name, server, input string
		want                time.Duration
	}{
		{"no deal_ids", "openx_mcp", `{"deal_id":"1"}`, toolCallTimeout},
		{"empty deal_ids", "openx_mcp", `{"deal_ids":[]}`, toolCallTimeout},
		{"unparseable input", "openx_mcp", `{"deal_ids":`, toolCallTimeout},
		{"floor: small batch", "openx_mcp", dealIDsInput(2), toolCallTimeout},
		{"floor: exactly 15 deals", "openx_mcp", dealIDsInput(15), toolCallTimeout},
		{"scaled: 59 deals", "openx_mcp", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
		{"scaled: variant server", "openx_mcp_tunnl", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
		{"cap: 200 deals", "openx_mcp", dealIDsInput(200), maxBatchToolCallTimeout},
		{"nexxen pace", "nexxen_mcp", dealIDsInput(12), 12 * nexxenBatchToolCallTimeoutPerDeal},
		{"nexxen variant pace", "nexxen_mcp_reklaim", dealIDsInput(12), 12 * nexxenBatchToolCallTimeoutPerDeal},
		{"nexxen floor", "nexxen_mcp", dealIDsInput(2), toolCallTimeout},
		{"nexxen cap", "nexxen_mcp", dealIDsInput(60), maxBatchToolCallTimeout},
		{"nexxen no deal_ids", "nexxen_mcp", `{"internal_deal_id":1}`, toolCallTimeout},
		{"nexxen-like name is not nexxen", "nexxen_mcpx", dealIDsInput(12), toolCallTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolCallTimeoutFor(tc.server, tc.input); got != tc.want {
				t.Fatalf("toolCallTimeoutFor(%q) = %v, want %v", tc.server, got, tc.want)
			}
		})
	}
}

// deadlineBroker records the deadline the MCP call context carried.
type deadlineBroker struct{ remaining time.Duration }

func (d *deadlineBroker) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.remaining = time.Until(dl)
	}
	return `{"ok":true}`, false, nil
}

// TestMCPToolAppliesScaledBatchTimeout pins the wiring: a deal_ids batch call
// runs under its scaled budget (plus the queue-wait backstop), not the flat
// default.
func TestMCPToolAppliesScaledBatchTimeout(t *testing.T) {
	broker := &deadlineBroker{}
	tool := &mcpTool{
		serverName: "openx_mcp",
		tool:       mcp.Tool{Name: "ox_merge_deal_domains", InputSchema: map[string]interface{}{"type": "object"}},
		broker:     broker,
	}
	input := dealIDsInput(59)
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Name: tool.Name(), Input: input}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := 59*batchToolCallTimeoutPerDeal + toolCallTimeout
	if broker.remaining <= want-time.Minute || broker.remaining > want {
		t.Fatalf("broker deadline = %v left, want ~%v (scaled 59-deal budget + queue backstop)", broker.remaining, want)
	}
}
