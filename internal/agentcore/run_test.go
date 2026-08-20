package agentcore

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// ── test doubles for the four seams ──

// stubInput supplies a fixed system prompt + one user message.
type stubInput struct {
	system string
	user   string
	label  string
}

func (s stubInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	return s.system, []fantasy.Message{fantasy.NewUserMessage(s.user)}, s.label, nil
}

// captureObserver records observed event types.
type captureObserver struct {
	events []string
}

func (o *captureObserver) Observe(eventType string, _ map[string]any) {
	o.events = append(o.events, eventType)
}

// stubExecutor is the Executor test double (the real sandbox backend is P3).
type stubExecutor struct {
	bashCalls   int32
	pythonCalls int32
}

func (e *stubExecutor) RunBash(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&e.bashCalls, 1)
	return "ok", nil
}

func (e *stubExecutor) RunPython(_ context.Context, _ string) (string, error) {
	atomic.AddInt32(&e.pythonCalls, 1)
	return "ok", nil
}

// roundCountingPolicy wraps a Policy and records how many CanFinish calls fired,
// so a test can assert the loop collapsed to one round.
type roundCountingPolicy struct {
	inner    Policy
	finishes int32
}

func (p *roundCountingPolicy) BeforeToolCall(t, id, in string) (bool, string) {
	return p.inner.BeforeToolCall(t, id, in)
}
func (p *roundCountingPolicy) RecordToolResult(t, in, out string, ok bool) {
	p.inner.RecordToolResult(t, in, out, ok)
}
func (p *roundCountingPolicy) CanFinish(round int) (bool, []string) {
	atomic.AddInt32(&p.finishes, 1)
	return p.inner.CanFinish(round)
}
func (p *roundCountingPolicy) orchestration() *orchestrationState {
	if op, ok := p.inner.(interface{ orchestration() *orchestrationState }); ok {
		return op.orchestration()
	}
	return nil
}

// TestInteractivePolicy_CanFinish_AlwaysRound1 verifies the 1-round collapse:
// with an InteractivePolicy (CanFinish true at round 0), Run executes exactly
// one pass against a fake provider, with a test-double Executor available.
func TestInteractivePolicy_CanFinish_AlwaysRound1(t *testing.T) {
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			return streamStop()(nil, call)
		},
	}
	policy := &roundCountingPolicy{inner: NewInteractivePolicy(0, 0, nil, nil)}
	obs := &captureObserver{}
	exec := &stubExecutor{}

	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		Temperature: 0.2,
	}, Deps{
		Input:    stubInput{system: "you are a test agent", user: "hello", label: "interactive-turn"},
		Observer: obs,
		Policy:   policy,
		Executor: exec,
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Rounds != 1 {
		t.Errorf("interactive run should collapse to 1 round, got %d", res.Rounds)
	}
	if got := atomic.LoadInt32(&policy.finishes); got != 1 {
		t.Errorf("CanFinish should be consulted exactly once, got %d", got)
	}
	if res.Label != "interactive-turn" {
		t.Errorf("label = %q, want interactive-turn", res.Label)
	}
}

// orchlessPolicy deliberately exposes NO orchestration state and no Unwrap.
type orchlessPolicy struct{}

func (orchlessPolicy) BeforeToolCall(string, string, string) (bool, string) { return false, "" }
func (orchlessPolicy) RecordToolResult(string, string, string, bool)        {}
func (orchlessPolicy) CanFinish(int) (bool, []string)                       { return true, nil }

// TestRun_RejectsPolicyWithoutOrchestration pins the fail-loud assertion
// (#1125): a Policy exposing no orchestrationState is a programming error Run
// refuses before the first provider call. The old fallback minted a fresh
// throwaway state per round, so such a run proceeded while its usage
// accumulated into objects nobody read — Result.Usage silently zero and the
// cost/token ceilings never firing.
func TestRun_RejectsPolicyWithoutOrchestration(t *testing.T) {
	model := &mockModel{}
	_, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  stubInput{system: "sys", user: "hello", label: "turn"},
		Policy: orchlessPolicy{},
		Model:  model,
	})
	if err == nil || !strings.Contains(err.Error(), "orchestration state") {
		t.Fatalf("Run must refuse a Policy without an orchestration state, got err=%v", err)
	}
	model.mu.Lock()
	calls := model.callCount
	model.mu.Unlock()
	if calls != 0 {
		t.Fatalf("refusal must precede any provider call, got %d calls", calls)
	}
}

// TestRun_RoundCapReturnsAccumulatedUsageAndEntries pins the #1125 fix for
// round-cap exhaustion: the max-enforcement-rounds error is still a hard
// error, but the rounds it spent are paid for — the accumulated Usage and
// streamed Entries must ride back on the Result instead of being dropped with
// an empty struct.
func TestRun_RoundCapReturnsAccumulatedUsageAndEntries(t *testing.T) {
	// The audit is never satisfied, so CanFinish blocks every round and the
	// loop exhausts maxEnforcementRounds.
	policy := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	model := &mockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			return func(yield func(fantasy.StreamPart) bool) {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "working"}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonStop,
					Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
				})
			}, nil
		},
	}

	res, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  stubInput{system: "sched", user: "do the task", label: "task-cap"},
		Policy: policy,
		Model:  model,
	})
	if err == nil || !strings.Contains(err.Error(), "max enforcement rounds") {
		t.Fatalf("expected the round-cap error, got %v", err)
	}
	if want := 50 * maxEnforcementRounds; res.Usage.PromptTokens != want {
		t.Errorf("Usage.PromptTokens = %d, want %d (accumulated across all capped rounds)", res.Usage.PromptTokens, want)
	}
	if want := 10 * maxEnforcementRounds; res.Usage.CompletionTokens != want {
		t.Errorf("Usage.CompletionTokens = %d, want %d", res.Usage.CompletionTokens, want)
	}
	if len(res.Entries) == 0 || res.FinalText == "" {
		t.Errorf("streamed transcript dropped on round-cap exhaustion: entries=%d finalText=%q", len(res.Entries), res.FinalText)
	}
	if res.Rounds != maxEnforcementRounds {
		t.Errorf("Rounds = %d, want %d", res.Rounds, maxEnforcementRounds)
	}
	if res.Label != "task-cap" {
		t.Errorf("Label = %q, want task-cap", res.Label)
	}
	if res.Cancelled {
		t.Error("round-cap exhaustion is a hard error, not a cancel")
	}
}

// TestScheduledPolicy_LoopsUntilAuditClears verifies the enforcement loop runs
// MORE than one round when the scheduled Policy blocks finishing: the first
// CanFinish is false (no audit), an enforcement nudge is injected, and the loop
// continues. We cap the model to stop so the loop terminates once the audit
// state is satisfied (set directly after round 0).
func TestScheduledPolicy_LoopsUntilAuditClears(t *testing.T) {
	policy := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	round := int32(0)
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			// Round 0's stream leaves the audit unsatisfied → first CanFinish
			// rejects and a nudge is injected. The SECOND stream satisfies the
			// audit so the round-1 CanFinish passes and the loop terminates.
			if atomic.AddInt32(&round, 1) == 2 {
				policy.orch.mu.Lock()
				policy.orch.selfAuditRequested = true
				policy.orch.selfAuditConfirmedOnce = true
				policy.orch.mu.Unlock()
			}
			return streamStop()(nil, call)
		},
	}

	res, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:  stubInput{system: "sched", user: "do the task", label: "task-1"},
		Policy: policy,
		Model:  model,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Rounds < 2 {
		t.Errorf("scheduled run should take >1 round when finish is blocked, got %d", res.Rounds)
	}
}
