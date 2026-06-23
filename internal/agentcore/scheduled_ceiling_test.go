package agentcore

import (
	"strings"
	"testing"
)

// TestScheduledPolicy_EnforcesCostCeiling is the regression guard for #75: a
// scheduled / one-shot run MUST enforce the configured cost ceiling (it
// previously enforced none — the dangerous case, since these run unattended).
func TestScheduledPolicy_EnforcesCostCeiling(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0.10 /* $0.10 ceiling */, 0)

	// Under the ceiling: a benign tool is allowed.
	if blocked, msg := p.BeforeToolCall("read_file", "c1", "{}"); blocked {
		t.Fatalf("blocked under the cost ceiling: %q", msg)
	}

	// Accumulated cost now meets/exceeds the ceiling → the next tool call blocks.
	p.orch.CostUSD = 0.50
	blocked, msg := p.BeforeToolCall("read_file", "c2", "{}")
	if !blocked || !strings.Contains(msg, "COST_CEILING_REACHED") {
		t.Fatalf("expected cost-ceiling block, got blocked=%v msg=%q", blocked, msg)
	}
}

// TestScheduledPolicy_EnforcesTokenCeiling guards the token half of #75.
func TestScheduledPolicy_EnforcesTokenCeiling(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0, 1000 /* 1000-token ceiling */)
	p.orch.PromptTokens = 900
	p.orch.CompletionTokens = 200 // 1100 uncached >= 1000
	blocked, msg := p.BeforeToolCall("read_file", "c1", "{}")
	if !blocked || !strings.Contains(msg, "TOKEN_CEILING_REACHED") {
		t.Fatalf("expected token-ceiling block, got blocked=%v msg=%q", blocked, msg)
	}
}

// TestScheduledPolicy_ZeroCeilingIsUnlimited confirms 0 disables the ceiling
// (back-compat for runs that intentionally configure no budget).
func TestScheduledPolicy_ZeroCeilingIsUnlimited(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	p.orch.CostUSD = 999
	p.orch.PromptTokens = 9_000_000
	if blocked, msg := p.BeforeToolCall("read_file", "c1", "{}"); blocked {
		t.Fatalf("zero ceiling must be unlimited, but it blocked: %q", msg)
	}
}

// TestStepStopConditions guards #74: the configured per-round step cap is turned
// into exactly one StopCondition (and 0/negative means "no cap"). The wiring of
// this into AgentStreamCall.StopWhen (engine.stream) is what makes
// MAX_ITERATIONS / a task's max_iterations actually bound the in-round tool loop.
func TestStepStopConditions(t *testing.T) {
	if stepStopConditions(0) != nil {
		t.Error("stepStopConditions(0) should be nil (no cap)")
	}
	if stepStopConditions(-5) != nil {
		t.Error("stepStopConditions(negative) should be nil (no cap)")
	}
	if got := stepStopConditions(100); len(got) != 1 {
		t.Fatalf("stepStopConditions(100): got %d conditions, want exactly 1", len(got))
	}
}
