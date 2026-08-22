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

// TestRegisterSecretLiteralScrubsNovelFormat is the regression test for the gap
// that made the parent's literal set empty of connector secrets.
//
// The tool-output redactor snapshots os.Environ() lazily, on first use. The MCP
// broker's boot path unsets every connector environment key long before that, so
// the snapshot never saw those values, and a connector echoing its own
// credential back was scrubbed only if the value happened to match one of
// internal/redact's shape patterns. The token below deliberately matches NONE of
// them — no sk-/ghp_/AKIA prefix, no "key=value" shape — so it is scrubbed only
// if literal registration actually reached the redactor.
//
// Ordering matters and is the thing under test: RegisterSecretLiteral is called
// BEFORE anything forces the redactor into existence, which is the real boot
// order (scrubParentConnectorState runs during broker startup, the first tool
// output comes much later).
func TestRegisterSecretLiteralScrubsNovelFormat(t *testing.T) {
	const novel = "Zq7Z2pLmVnT4rWxK9dCbYeHgJ1sAuF6o"

	RegisterSecretLiteral(novel)

	got := RedactSecrets("upstream said: token " + novel + " was rejected")
	if strings.Contains(got, novel) {
		t.Fatalf("novel-format secret survived redaction: %q", got)
	}
	if !strings.Contains(got, "upstream said") {
		t.Fatalf("redaction ate the surrounding text: %q", got)
	}

	// Registering after construction must work too — runtime-acquired
	// credentials (OAuth bearers, unsealed api_keys) arrive mid-process via
	// Service.SetSecretObserver, i.e. long after the redactor exists.
	const later = "Rr8Y3qMnWoU5sXyL0eDcZfIhK2tBvG7p"
	RegisterSecretLiteral(later)
	if out := RedactSecrets("bearer " + later); strings.Contains(out, later) {
		t.Fatalf("post-construction secret survived redaction: %q", out)
	}

	// An empty registration must not turn the scrubber into a match-everything.
	RegisterSecretLiteral("")
	if out := RedactSecrets("ordinary text"); out != "ordinary text" {
		t.Fatalf("empty literal corrupted redaction: %q", out)
	}
}
