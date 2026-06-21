package agentcore

import (
	"context"
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

// TestScheduledPolicy_LoopsUntilAuditClears verifies the enforcement loop runs
// MORE than one round when the scheduled Policy blocks finishing: the first
// CanFinish is false (no audit), an enforcement nudge is injected, and the loop
// continues. We cap the model to stop so the loop terminates once the audit
// state is satisfied (set directly after round 0).
func TestScheduledPolicy_LoopsUntilAuditClears(t *testing.T) {
	policy := NewScheduledPolicy(NewLogSession(), 50)
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
