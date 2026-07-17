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
	if len(resp.Content) > ceil {
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

	if !strings.Contains(resp.Content, "original_bytes: 200013") || !strings.Contains(resp.Content, "recovery_action:") {
		t.Errorf("direct-path envelope is missing size/recovery metadata: %q", resp.Content[:min(len(resp.Content), 300)])
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
	if len(directResp.Content) > ceil || len(deferredResp.Content) > ceil {
		t.Errorf("both paths must be capped near the %d-byte ceiling; got direct=%d deferred=%d",
			ceil, len(directResp.Content), len(deferredResp.Content))
	}
}

func TestMCPGuardrailRunsExactlyOnceDirectAndDeferred(t *testing.T) {
	t.Cleanup(func() {
		SetGuardrail(false, false, "off", "", nil)
		SetToolDisclosureThreshold(0)
	})
	catalog := []mcp.ServerTool{{ServerName: "srv", Tool: mcp.Tool{
		Name: "dump", Description: "result", InputSchema: map[string]any{"type": "object"},
	}}}
	broker := &hugeBroker{payload: "ordinary connector result"}
	core := fantasy.NewAgentTool("core", "core", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})

	for _, tc := range []struct {
		name      string
		threshold int
		native    []fantasy.AgentTool
		tool      string
		input     string
	}{
		{name: "direct", threshold: 100, tool: "mcp_srv_dump", input: `{}`},
		{name: "deferred", threshold: 1, native: []fantasy.AgentTool{core}, tool: "tool_call", input: `{"name":"mcp_srv_dump","arguments":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detector := &fakeGuardrailDetector{}
			SetGuardrail(true, false, "observe", "prompt-injection", detector)
			SetToolDisclosureThreshold(tc.threshold)
			registered, err := buildFantasyTools(tc.native, catalog, broker, nil, nil, nil, nil, toolBuildConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := findRegisteredTool(registered, tc.tool).Run(context.Background(), fantasy.ToolCall{ID: tc.name, Name: tc.tool, Input: tc.input}); err != nil {
				t.Fatal(err)
			}
			if detector.calls != 1 {
				t.Fatalf("guardrail calls=%d, want exactly one", detector.calls)
			}
		})
	}
}
