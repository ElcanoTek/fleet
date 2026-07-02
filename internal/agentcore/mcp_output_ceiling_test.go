package agentcore

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// hugeBroker returns an oversized MCP payload (well past the default 64 KB
// output ceiling) with a needle in the middle so tests can prove the middle
// was dropped.
type hugeBroker struct{ payload string }

func (h *hugeBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return h.payload, false, nil
}

// TestMCPDirectPathAppliesOutputCeiling is the #576 regression: below the
// tool-disclosure threshold MCP tools register as raw *mcpTool, whose Run used
// to apply redaction + PII but NOT the #199 output ceiling — so a connector
// dumping hundreds of KB entered the transcript untruncated and overflowed the
// context window (triggering the wrong-message reactive compaction the ceiling
// exists to prevent). The direct path must now truncate exactly like the
// deferred (tool_call) path always did.
func TestMCPDirectPathAppliesOutputCeiling(t *testing.T) {
	payload := strings.Repeat("A", 100_000) + "MIDDLE_NEEDLE" + strings.Repeat("B", 100_000)
	broker := &hugeBroker{payload: payload}

	direct := &mcpTool{
		serverName: "srv",
		tool: mcp.Tool{
			Name:        "dump",
			Description: "returns a huge payload",
			InputSchema: map[string]interface{}{"type": "object"},
		},
		broker: broker,
	}

	resp, err := direct.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Name: direct.Name(), Input: "{}"})
	if err != nil {
		t.Fatalf("direct Run: %v", err)
	}
	if resp.IsError {
		t.Fatalf("direct Run returned IsError: %s", resp.Content)
	}

	ceil := maxToolOutputBytes()
	if len(resp.Content) > ceil+512 { // + marker slack
		t.Fatalf("direct path did not truncate: %d bytes flowed on (ceiling %d)", len(resp.Content), ceil)
	}
	if strings.Contains(resp.Content, "MIDDLE_NEEDLE") {
		t.Error("the middle of an oversized payload should have been dropped")
	}
	if !strings.Contains(resp.Content, "truncated") {
		t.Error("a truncation marker should be present")
	}
	if !utf8.ValidString(resp.Content) {
		t.Error("truncated output must remain valid UTF-8")
	}

	// The exact bytes must match what the shared ceiling produces — one cap, one
	// behavior, no drift between paths.
	want, truncated := applyOutputCeiling(payload, ceil)
	if !truncated {
		t.Fatal("test payload must exceed the ceiling")
	}
	if resp.Content != want {
		t.Errorf("direct-path truncation diverges from applyOutputCeiling (len %d vs %d)", len(resp.Content), len(want))
	}
}

// TestMCPCeilingParityDirectVsDeferred asserts the SAME oversized MCP result is
// truncated identically whether the tool was registered directly (below the
// disclosure threshold) or dispatched via the wrapped tool_call bridge (above
// it) — "disclosure changes visibility, not governance".
func TestMCPCeilingParityDirectVsDeferred(t *testing.T) {
	payload := strings.Repeat("A", 100_000) + "MIDDLE_NEEDLE" + strings.Repeat("B", 100_000)
	broker := &hugeBroker{payload: payload}

	serverTools := []mcp.ServerTool{{ServerName: "srv", Tool: mcp.Tool{
		Name:        "dump",
		Description: "returns a huge payload",
		InputSchema: map[string]interface{}{"type": "object"},
	}}}

	// Direct path: the raw *mcpTool exactly as buildFantasyTools registers it
	// below the threshold.
	deferred := mcpToolsFrom(serverTools, broker)
	directResp, err := deferred[0].Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Name: "mcp_srv_dump", Input: "{}"})
	if err != nil {
		t.Fatalf("direct Run: %v", err)
	}

	// Deferred path: the tool_call bridge wrapped in policyGuardedTool, exactly
	// as buildFantasyTools registers the bridges above the threshold.
	reg := newDeferredToolRegistry(deferred)
	bridge := &policyGuardedTool{inner: reg.callTool()}
	deferredResp, err := bridge.Run(context.Background(), fantasy.ToolCall{ID: "tc-2", Name: "tool_call", Input: `{"name":"mcp_srv_dump","arguments":{}}`})
	if err != nil {
		t.Fatalf("deferred Run: %v", err)
	}

	if directResp.Content != deferredResp.Content {
		t.Errorf("direct (%d bytes) and deferred (%d bytes) paths truncate differently — the ceiling must apply identically on both",
			len(directResp.Content), len(deferredResp.Content))
	}
	ceil := maxToolOutputBytes()
	if len(directResp.Content) > ceil+512 || len(deferredResp.Content) > ceil+512 {
		t.Errorf("both paths must be capped near the %d-byte ceiling; got direct=%d deferred=%d",
			ceil, len(directResp.Content), len(deferredResp.Content))
	}
}
