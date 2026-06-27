package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type redactTestInput struct{}

// TestPolicyGuardedTool_RedactsToolOutput drives a native tool whose output
// contains a secret through the real policyGuardedTool wrapper and asserts the
// returned content — which is what re-enters the model context, the stream, and
// the log — is scrubbed (#307).
func TestPolicyGuardedTool_RedactsToolOutput(t *testing.T) {
	inner := fantasy.NewAgentTool(
		"bash", "bash",
		func(_ context.Context, _ redactTestInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("export OPENAI_API_KEY=sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345\nall done"), nil
		},
	)
	guarded := &policyGuardedTool{inner: inner, policy: nil}

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "t1", Input: "{}"})
	if err != nil {
		t.Fatalf("guarded run: %v", err)
	}
	if strings.Contains(resp.Content, "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345") {
		t.Errorf("secret survived tool output: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in tool output: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "all done") {
		t.Errorf("redaction ate surrounding output: %q", resp.Content)
	}
}
