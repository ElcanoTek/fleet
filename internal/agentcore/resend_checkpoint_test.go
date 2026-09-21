package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

type tickInput struct {
	N int `json:"n"`
}

// toolStep streams one tick call with a distinct argument per step: the run's
// loop guard blocks an identical call repeated more than three times, which is
// not what this test is about.
func toolStep(usage fantasy.Usage, id string, n int32) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: "tick", ToolCallInput: fmt.Sprintf(`{"n":%d}`, n)})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: usage})
	}
}

func TestStepAtResendBudget(t *testing.T) {
	step := func(reason fantasy.FinishReason, in, cached int64) fantasy.StepResult {
		return fantasy.StepResult{Response: fantasy.Response{FinishReason: reason, Usage: fantasy.Usage{InputTokens: in, CacheReadTokens: cached}}}
	}
	cases := []struct {
		name  string
		steps []fantasy.StepResult
		want  bool
	}{
		{"no steps", nil, false},
		{"tool step under budget", []fantasy.StepResult{step(fantasy.FinishReasonToolCalls, 500, 0)}, false},
		{"tool step at budget counts cache reads", []fantasy.StepResult{step(fantasy.FinishReasonToolCalls, 400, 600)}, true},
		{"tool step over budget", []fantasy.StepResult{step(fantasy.FinishReasonToolCalls, 5000, 0)}, true},
		{"final answer over budget never pauses", []fantasy.StepResult{step(fantasy.FinishReasonStop, 5000, 0)}, false},
		{"only the last step matters", []fantasy.StepResult{step(fantasy.FinishReasonToolCalls, 5000, 0), step(fantasy.FinishReasonToolCalls, 100, 0)}, false},
	}
	for _, tc := range cases {
		if got := stepAtResendBudget(tc.steps, 1000); got != tc.want {
			t.Errorf("%s: stepAtResendBudget = %v, want %v", tc.name, got, tc.want)
		}
	}
	if stepAtResendBudget([]fantasy.StepResult{step(fantasy.FinishReasonToolCalls, 5000, 0)}, 0) {
		t.Error("a zero budget must never pause")
	}
}

func TestResendBudgetCheckpoint_ScheduledOnlyAndBudgetGated(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	model := &namedMockModel{name: "cp-gate"}
	interactive := newMockEngine(t, model)
	interactive.envPrefix = CanonicalEnvPrefix
	if interactive.resendBudgetCheckpoint() != nil {
		t.Error("an interactive engine must not get a resend checkpoint: a chat's history is the user's to keep")
	}
	if got := len(interactive.roundStopConditions(0)); got != 0 {
		t.Errorf("interactive round stop conditions = %d, want 0", got)
	}
	scheduled := newMockEngine(t, model)
	scheduled.envPrefix = CanonicalEnvPrefix
	scheduled.requireCompactionOptIn = true
	if scheduled.resendBudgetCheckpoint() == nil {
		t.Fatal("a scheduled engine with a resend budget must get the checkpoint")
	}
	if got := len(scheduled.roundStopConditions(7)); got != 2 {
		t.Errorf("scheduled round stop conditions = %d, want step cap + checkpoint", got)
	}
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	if scheduled.resendBudgetCheckpoint() != nil {
		t.Error("budget 0 disables the checkpoint")
	}
}

// A scheduled run whose tool loop keeps resending a prompt over the budget
// must pause at the checkpoint, compact, and RESUME the same work — every tool
// step still executes exactly once and the run still ends with the final
// answer. Before this, one fantasy round ran the whole loop and the cost-aware
// compaction fired at most once per verifier round.
func TestRun_ResendCheckpoint_ScheduledPausesCompactsAndResumes(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	const toolSteps = 6
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance the fake work by one step",
		func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ticks.Add(1)
			return fantasy.NewTextResponse(strings.Repeat("r", 400)), nil
		})
	model := newStopModel("cp-run")
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		n := calls.Add(1)
		if int(n) <= toolSteps {
			// Every tool step's prompt is already over the 1000-token budget.
			return toolStep(fantasy.Usage{InputTokens: 1500, OutputTokens: 10}, fmt.Sprintf("call-%d", n), n), nil
		}
		return streamTextThenFinish("all six steps done"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeScheduled,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true, NativeTools: []fantasy.AgentTool{tick}},
		Deps{
			Input:      historyInput{system: "s", msgs: fillerMessages(2, 200), label: "sched"},
			Observer:   obs,
			Policy:     finishableScheduledPolicy(session),
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ticks.Load(); got != toolSteps {
		t.Fatalf("tool ran %d times, want %d — a checkpoint must never drop or replay a step (final=%q)", got, toolSteps, res.FinalText)
	}
	if !strings.Contains(res.FinalText, "all six steps done") {
		t.Fatalf("final text = %q, want the model's closing answer", res.FinalText)
	}
	checkpoints, compactions := 0, 0
	for _, e := range obs.events {
		switch e {
		case evtContextCheckpoint:
			checkpoints++
		case evtContextCompacted:
			compactions++
		}
	}
	if checkpoints < toolSteps-1 {
		t.Errorf("checkpoints = %d, want one per over-budget tool step (>= %d); events=%v", checkpoints, toolSteps-1, obs.events)
	}
	if compactions == 0 {
		t.Errorf("expected at least one resend_budget compaction after a checkpoint, events=%v", obs.events)
	}
	if p := obs.payloadOf(evtContextCheckpoint); p == nil || p[evtFieldTrigger] != "resend_budget" || p[evtFieldResendBudget] != 1000 || p[evtFieldUsedTokens] != 1500 {
		t.Errorf("checkpoint payload must carry trigger, budget and the resent size, got %v", p)
	}
	var crumb bool
	for _, m := range session.Messages {
		if strings.Contains(m.Content, "[context_checkpoint] resent prompt 1500 tokens") {
			crumb = true
		}
	}
	if !crumb {
		t.Error("expected a [context_checkpoint] breadcrumb in the session log")
	}
}

// Past the checkpoint cap the pause is ignored: the round falls through to the
// ordinary not-finished path and the run still completes.
func TestRun_ResendCheckpoint_CapDoesNotStrandTheRun(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	steps := maxResendCheckpoints + 3
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance", func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		ticks.Add(1)
		return fantasy.NewTextResponse("ok"), nil
	})
	model := newStopModel("cp-cap")
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		if n := calls.Add(1); int(n) <= steps {
			return toolStep(fantasy.Usage{InputTokens: 1500, OutputTokens: 1}, fmt.Sprintf("c-%d", n), n), nil
		}
		return streamTextThenFinish("done after the cap"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeScheduled,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true, NativeTools: []fantasy.AgentTool{tick}},
		Deps{Input: historyInput{system: "s", msgs: fillerMessages(1, 50), label: "sched"}, Observer: obs, Policy: finishableScheduledPolicy(session), Model: model, LogSession: session})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if int(ticks.Load()) != steps || !strings.Contains(res.FinalText, "done after the cap") {
		t.Fatalf("ticks=%d final=%q, want %d steps and the closing answer", ticks.Load(), res.FinalText, steps)
	}
	checkpoints := 0
	for _, e := range obs.events {
		if e == evtContextCheckpoint {
			checkpoints++
		}
	}
	if checkpoints != maxResendCheckpoints {
		t.Errorf("checkpoints = %d, want exactly the cap %d", checkpoints, maxResendCheckpoints)
	}
}

// The step cap is a run-wide bound on the tool loop, not a per-pause allowance:
// with MaxIterations=4 and every step over the budget, exactly four steps run
// and the fourth pause is refused so the cap's own handling takes over.
func TestRun_ResendCheckpoint_StepCapHoldsAcrossCheckpoints(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "advance", func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		ticks.Add(1)
		return fantasy.NewTextResponse("ok"), nil
	})
	model := newStopModel("cp-stepcap")
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		n := calls.Add(1)
		if int(n) <= 20 {
			return toolStep(fantasy.Usage{InputTokens: 1500, OutputTokens: 1}, fmt.Sprintf("s-%d", n), n), nil
		}
		return streamTextThenFinish("never reached"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	_, _ = Run(context.Background(), ModeScheduled,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true, MaxIterations: 4, NativeTools: []fantasy.AgentTool{tick}},
		Deps{Input: historyInput{system: "s", msgs: fillerMessages(1, 50), label: "sched"}, Observer: obs, Policy: finishableScheduledPolicy(session), Model: model, LogSession: session})
	checkpoints := 0
	for _, e := range obs.events {
		if e == evtContextCheckpoint {
			checkpoints++
		}
	}
	if got := ticks.Load(); got > 4 {
		t.Fatalf("tool ran %d times with MaxIterations=4 — checkpoints must not reset the step cap", got)
	}
	if checkpoints != 3 {
		t.Errorf("checkpoints = %d, want 3 (the fourth step is the cap's, not a pause)", checkpoints)
	}
}

func TestConsumeResendCheckpoint_CapTieRefusesAndAccountingResets(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	e := newMockEngine(t, &namedMockModel{name: "cp-tie"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.maxIterations = 3
	over := func(n int) *fantasy.AgentResult {
		r := &fantasy.AgentResult{}
		for i := 0; i < n; i++ {
			r.Steps = append(r.Steps, fantasy.StepResult{Response: fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 1500}}})
		}
		return r
	}
	if !e.consumeResendCheckpoint(over(1), 1) {
		t.Fatal("first single-step pause fits under a cap of 3")
	}
	if !e.consumeResendCheckpoint(over(1), 1) {
		t.Fatal("second single-step pause fits under a cap of 3")
	}
	if e.consumeResendCheckpoint(over(1), 1) {
		t.Fatal("the third step reaches the cap: the pause must be refused so the cap wins the tie")
	}
	e.roundEndedOnItsOwn()
	if !e.consumeResendCheckpoint(over(1), 1) {
		t.Fatal("a round that ended on its own resets the step accounting for the next logical round")
	}
	if e.checkpointSteps != 1 {
		t.Errorf("checkpointSteps = %d, want 1 after the reset and one pause", e.checkpointSteps)
	}
}

// A resilience recovery resumes past completed steps, so the final attempt's
// result holds only the tail of the round; the checkpoint accounting must count
// the whole round or the next stream gets nearly the full cap again.
func TestConsumeResendCheckpoint_CountsResilienceResumedSteps(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	e := newMockEngine(t, &namedMockModel{name: "cp-resumed"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.maxIterations = 100
	tail := &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 1500}}}}}
	// 99 steps completed before the recovery + 1 in the resumed attempt = the cap.
	if e.consumeResendCheckpoint(tail, 99+len(tail.Steps)) {
		t.Fatal("a recovered round that reached the cap must not be paused")
	}
	if !e.consumeResendCheckpoint(tail, 50+len(tail.Steps)) {
		t.Fatal("a recovered round under the cap may pause")
	}
	if e.checkpointSteps != 51 {
		t.Errorf("checkpointSteps = %d, want the round's TOTAL 51, not the tail's 1", e.checkpointSteps)
	}
}

// Compaction summaries follow the model the run is driving: after a swap the
// input names the fallback, not the configured primary.
func TestCompactionSummarizeInput_FollowsTheActiveModel(t *testing.T) {
	primary := &namedMockModel{name: "primary"}
	fallback := &namedMockModel{name: "fallback"}
	e := newMockEngine(t, primary)
	if in := e.compactionSummarizeInput(nil); in.Model == nil || in.Model.Model() != "primary" {
		t.Fatalf("before any swap the summary input must name the primary, got %v", in.Model)
	}
	e.activeModel = fallback
	if in := e.compactionSummarizeInput(nil); in.Model == nil || in.Model.Model() != "fallback" {
		t.Fatalf("after a swap the summary input must name the fallback, got %v", in.Model)
	}
}
