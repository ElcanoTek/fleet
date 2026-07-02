package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/piiredact"
)

type piiTestInput struct{}

// toolReturning builds a native tool whose output is fixed, to drive the real
// policyGuardedTool wrapper (the tool-output choke point where #450 hooks in).
func toolReturning(output string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"lookup", "lookup",
		func(_ context.Context, _ piiTestInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(output), nil
		},
	)
}

func runGuarded(t *testing.T, output string) fantasy.ToolResponse {
	t.Helper()
	guarded := &policyGuardedTool{inner: toolReturning(output), policy: nil}
	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "t1", Input: "{}"})
	if err != nil {
		t.Fatalf("guarded run: %v", err)
	}
	return resp
}

// TestPIIRedaction_DisabledByDefault: with no redactor installed, tool output
// with PII flows through unchanged (byte-for-byte the pre-#450 behavior).
func TestPIIRedaction_DisabledByDefault(t *testing.T) {
	SetPIIRedactor(nil)
	resp := runGuarded(t, "customer email is jane@corp.com")
	if !strings.Contains(resp.Content, "jane@corp.com") {
		t.Errorf("PII should pass through when redaction is off: %q", resp.Content)
	}
	if resp.IsError {
		t.Error("off mode should not mark the result an error")
	}
}

// TestPIIRedaction_RedactMode: an installed redact-mode redactor masks PII in the
// tool output that re-enters the model context.
func TestPIIRedaction_RedactMode(t *testing.T) {
	SetPIIRedactor(piiredact.New(piiredact.ModeRedact))
	t.Cleanup(func() { SetPIIRedactor(nil) })

	resp := runGuarded(t, "customer email is jane@corp.com, ticket resolved")
	if strings.Contains(resp.Content, "jane@corp.com") {
		t.Errorf("email survived redaction: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "[PII:email]") {
		t.Errorf("expected [PII:email] marker: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "ticket resolved") {
		t.Errorf("redaction ate surrounding text: %q", resp.Content)
	}
	if resp.IsError {
		t.Error("redact mode should not mark the result an error")
	}
}

// TestPIIRedaction_ObserveMode: observe passes the text through unchanged (the
// audit log records findings; behavior is unaffected).
func TestPIIRedaction_ObserveMode(t *testing.T) {
	SetPIIRedactor(piiredact.New(piiredact.ModeObserve))
	t.Cleanup(func() { SetPIIRedactor(nil) })

	resp := runGuarded(t, "reach ops at ops@corp.com")
	if !strings.Contains(resp.Content, "ops@corp.com") {
		t.Errorf("observe mode must not modify output: %q", resp.Content)
	}
	if resp.IsError {
		t.Error("observe mode should not mark the result an error")
	}
}

// TestPIIRedaction_BlockMode: block withholds the whole result and flags it as an
// error so the raw value never reaches the model.
func TestPIIRedaction_BlockMode(t *testing.T) {
	SetPIIRedactor(piiredact.New(piiredact.ModeBlock))
	t.Cleanup(func() { SetPIIRedactor(nil) })

	resp := runGuarded(t, "SSN 123-45-6789 for the account")
	if strings.Contains(resp.Content, "123-45-6789") {
		t.Errorf("block mode leaked the raw value: %q", resp.Content)
	}
	if !strings.HasPrefix(resp.Content, "[BLOCKED:") {
		t.Errorf("block mode should withhold with a notice: %q", resp.Content)
	}
	if !resp.IsError {
		t.Error("block mode should mark the tool result an error")
	}
}

// TestPIIRedaction_CleanOutputUnaffected: output with no PII is untouched in any
// mode, and the secret scrubber still runs alongside.
func TestPIIRedaction_CleanOutputUnaffected(t *testing.T) {
	SetPIIRedactor(piiredact.New(piiredact.ModeRedact))
	t.Cleanup(func() { SetPIIRedactor(nil) })

	resp := runGuarded(t, "the report is ready")
	if resp.Content != "the report is ready" || resp.IsError {
		t.Errorf("clean output should be untouched, got %q (isErr=%v)", resp.Content, resp.IsError)
	}
}
