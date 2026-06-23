package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
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

// TestCeiling_HardAbortBeforeNextCompletion is the regression guard for #82: when
// the run's accumulated usage crosses the ceiling, the budget-guarded PrepareStep
// aborts BEFORE the next paid completion and the run finishes GRACEFULLY with the
// partial transcript — it does NOT keep stepping until maxEnforcementRounds.
func TestCeiling_HardAbortBeforeNextCompletion(t *testing.T) {
	// 10-token ceiling; the audit is NEVER satisfied, so without the hard abort
	// the loop would run to maxEnforcementRounds and return an error.
	policy := NewScheduledPolicy(NewLogSession(), 50, 0, 10)
	model := &mockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "working"})
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonStop,
					Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10}, // > the 10-token ceiling
				})
			}, nil
		},
	}
	res, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    stubInput{system: "sys", user: "go", label: "sched"},
		Observer: &captureObserver{},
		Policy:   policy,
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("ceiling hit should finish gracefully, got error: %v", err)
	}
	if res.Rounds >= maxEnforcementRounds {
		t.Fatalf("run hit the round cap (%d) instead of aborting on the ceiling — hard abort not engaged", res.Rounds)
	}
	if res.FinalText == "" {
		t.Error("graceful ceiling stop should still return the partial transcript")
	}
}
