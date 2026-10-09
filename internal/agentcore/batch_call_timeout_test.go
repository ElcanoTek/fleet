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
// toolCallTimeout and capped at maxBatchToolCallTimeout. The pace is the
// engine default unless the bundle declares the server's own
// (batch_seconds_per_deal), which also governs that server's named-account
// variants — and no server is paced by its name alone.
func TestToolCallTimeoutFor(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{BatchSecondsPerDeal: map[string]int{
		"paced_mcp":      45,
		"paced_mcp_slow": 90,
		"ignored_mcp":    0,
		"negative_mcp":   -5,
	}})
	const paced = 45 * time.Second
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
		{"declared pace", "paced_mcp", dealIDsInput(12), 12 * paced},
		{"declared pace: variant inherits base", "paced_mcp_reklaim", dealIDsInput(12), 12 * paced},
		{"declared pace: variant's own key wins", "paced_mcp_slow", dealIDsInput(12), 12 * 90 * time.Second},
		{"declared pace: floor", "paced_mcp", dealIDsInput(2), toolCallTimeout},
		{"declared pace: cap", "paced_mcp", dealIDsInput(60), maxBatchToolCallTimeout},
		{"declared pace: no deal_ids", "paced_mcp", `{"internal_deal_id":1}`, toolCallTimeout},
		{"declared pace: look-alike name is not governed", "paced_mcpx", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
		{"zero pace ignored", "ignored_mcp", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
		{"negative pace ignored", "negative_mcp", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
		{"no name-based pacing: nexxen without a declaration", "nexxen_mcp", dealIDsInput(59), 59 * batchToolCallTimeoutPerDeal},
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
