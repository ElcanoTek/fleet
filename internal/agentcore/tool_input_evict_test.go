package agentcore

// Review pins for the #793 x #788 composition: oversized tool-call-input
// eviction (replacing coverage deleted with internal/agent/overflow_test.go),
// hook fragments/reasons that look like encoded binary, and MCP post-call
// governance surviving an expired callCtx.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/guardrail"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// base64ish returns a single base64-alphabet run long enough to trip
// containsEncodedBinary (>=256 chars, mixed categories).
func base64ish() string {
	return strings.Repeat("QmFzZTY0U2lnbmF0dXJlQmxvYjEyMzQ1Njc4OTBhYmNkZWZnaGlqa2xtbm9w", 6)
}

func TestReduceHistoricalPayloads_EvictsOversizedToolCallInput(t *testing.T) {
	big := `{"data":"` + strings.Repeat("x", HardMaxToolOutputBytes+1024) + `"}`
	small := `{"page":1}`
	messages := []fantasy.Message{{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: big},
			fantasy.ToolCallPart{ToolCallID: "c2", ToolName: "bash", Input: small},
		},
	}}
	reduced := reduceHistoricalPayloadsToHardCap(messages, map[string]string{})
	if reduced.inputEvicts != 1 {
		t.Fatalf("inputEvicts = %d, want 1", reduced.inputEvicts)
	}
	p1, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](messages[0].Content[0])
	p2, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](messages[0].Content[1])
	if p2.Input != small {
		t.Fatalf("small input rewritten: %q", p2.Input)
	}
	if len(p1.Input) > innerInputEvictedBytes {
		t.Fatalf("evicted input %d bytes > cap %d", len(p1.Input), innerInputEvictedBytes)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(p1.Input), &env); err != nil {
		t.Fatalf("eviction envelope is not valid JSON: %v\n%s", err, p1.Input)
	}
	if env["_fleet_context_evicted"] != true {
		t.Fatalf("envelope missing _fleet_context_evicted: %v", env)
	}
	// The call already ran; the envelope must not tell the model to re-run a
	// possibly side-effectful call.
	action, _ := env["recovery_action"].(string)
	if !strings.Contains(action, "do NOT repeat") {
		t.Fatalf("recovery_action invites re-execution: %q", action)
	}
}

func TestEvictOldToolInputs_StopsAtTarget(t *testing.T) {
	mkMsg := func(id string) fantasy.Message {
		return fantasy.Message{
			Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{
				fantasy.ToolCallPart{ToolCallID: id, ToolName: "bash", Input: `{"d":"` + strings.Repeat("y", 8*1024) + `"}`},
			},
		}
	}
	messages := []fantasy.Message{mkMsg("a"), mkMsg("b"), mkMsg("c")}
	// Impossible target: everything evictable must be evicted.
	if n, _ := evictOldToolInputs(messages, estimateBudgetMessagesTokens(messages, modelContextPrefixBudget{}), 0); n != 3 {
		t.Fatalf("evicted %d, want all 3", n)
	}
	// Generous target: nothing should be touched.
	messages = []fantasy.Message{mkMsg("a")}
	if n, _ := evictOldToolInputs(messages, estimateBudgetMessagesTokens(messages, modelContextPrefixBudget{}), 1<<30); n != 0 {
		t.Fatalf("evicted %d under a generous target, want 0", n)
	}
}

func TestAggregateInputEnvelope_FallsBackWhenOverMax(t *testing.T) {
	out := aggregateInputEnvelope(strings.Repeat("n", 600), 999, 64)
	if len(out) > 128 {
		t.Fatalf("fallback envelope too large: %d bytes", len(out))
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("fallback envelope invalid JSON: %v", err)
	}
	if env["_fleet_context_evicted"] != true {
		t.Fatalf("fallback envelope missing marker: %s", out)
	}
}

func TestHookEngine_Base64FragmentDroppedNotSuppressing(t *testing.T) {
	exec := &scriptExecutor{out: `{"decision":"continue","additional_context":"attestation: ` + base64ish() + `"}`}
	e := engineWith(t, exec, nil, LifecycleHook{ID: "h", Event: HookPostToolUse})
	if frag := e.postToolUse(context.Background(), "bash", "c1", "{}", "ok", false); frag != "" {
		t.Fatalf("base64-heavy fragment must be dropped, got %d bytes", len(frag))
	}
	// A normal fragment still flows.
	exec.out = `{"decision":"continue","additional_context":"lint clean"}`
	if frag := e.postToolUse(context.Background(), "bash", "c2", "{}", "ok", false); !strings.Contains(frag, "lint clean") {
		t.Fatalf("plain fragment lost: %q", frag)
	}
}

func TestHookEngine_Base64BlockReasonWithheldButStillBlocks(t *testing.T) {
	exec := &scriptExecutor{out: `{"decision":"block","reason":"sig ` + base64ish() + `"}`}
	e := engineWith(t, exec, nil, LifecycleHook{ID: "sig-check", Event: HookPreToolUse})
	blocked, reason := e.preToolUse(context.Background(), "bash", "c1", "{}")
	if !blocked {
		t.Fatal("block decision lost")
	}
	if containsEncodedBinary(reason) {
		t.Fatalf("reason still binary-ish; would be suppression-enveloped: %q", reason)
	}
	if !strings.Contains(reason, "sig-check") {
		t.Fatalf("withheld reason should name the hook: %q", reason)
	}
}

// ctxErrDetector honors context cancellation the way the production HTTP
// detector does (http.NewRequestWithContext fails on a dead context).
type ctxErrDetector struct{}

func (ctxErrDetector) Check(ctx context.Context, _, _, _ string) (guardrail.Verdict, error) {
	if err := ctx.Err(); err != nil {
		return guardrail.Verdict{}, err
	}
	return guardrail.Verdict{}, nil
}

// hangingBroker blocks until the per-call ctx dies, like a hung stdio server.
type hangingBroker struct{}

func (hangingBroker) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}
func (hangingBroker) Close() error { return nil }

func TestMCPTool_TimeoutErrorSurvivesExpiredCallCtx(t *testing.T) {
	prevTimeout := toolCallTimeout
	toolCallTimeout = 30 * time.Millisecond
	t.Cleanup(func() { toolCallTimeout = prevTimeout })
	SetGuardrail(true, true, "block", "p", ctxErrDetector{})
	t.Cleanup(func() { SetGuardrail(false, false, "", "", nil) })

	tool := &mcpTool{serverName: "srv", tool: mcp.Tool{Name: "hang"}, broker: hangingBroker{}}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: "{}"})
	if err != nil {
		t.Fatalf("transport errors surface as error responses, got err: %v", err)
	}
	if !resp.IsError {
		t.Fatal("expected an error response")
	}
	// The parent ctx is alive: the block-mode guardrail must screen the REAL
	// transport error, not fail closed on the expired callCtx and rewrite the
	// timeout into a fake "[BLOCKED: workspace content guardrail]".
	if strings.Contains(resp.Content, "BLOCKED") {
		t.Fatalf("timeout rewritten into a guardrail block: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "Error calling") {
		t.Fatalf("transport error text lost: %q", resp.Content)
	}
}
