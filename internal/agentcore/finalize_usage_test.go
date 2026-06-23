package agentcore

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

// TestFinalizeHook_RecordUsageFlowsIntoRunUsage is the regression guard for #83:
// a finalize hook that makes its own recovery model call meters that call's
// tokens via FinalizeInput.RecordUsage, and those tokens land in the SAME run
// accounting the main loop uses (so the cost chip is not undercounted). The
// recovery usage is wired through a capability closure over the run's
// orchestration state — the state never escapes Run.
func TestFinalizeHook_RecordUsageFlowsIntoRunUsage(t *testing.T) {
	model := &mockModel{
		streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
			return streamStop()(nil, call) // main pass: 50 in / 10 out
		},
	}
	var sawSink bool
	res, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    stubInput{system: "sys", user: "hi", label: "t"},
		Observer: &captureObserver{},
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
		Finalize: func(_ context.Context, in FinalizeInput) (string, error) {
			if in.RecordUsage == nil {
				t.Fatal("FinalizeInput.RecordUsage was not wired by Run")
			}
			sawSink = true
			// Simulate the recovery call's metered step.
			in.RecordUsage(fantasy.Usage{InputTokens: 7, OutputTokens: 3}, fantasy.ProviderMetadata{})
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawSink {
		t.Fatal("finalize hook never ran")
	}
	// Main pass (50/10) + the recovery step (7/3) must BOTH be counted.
	if res.Usage.PromptTokens != 57 || res.Usage.CompletionTokens != 13 {
		t.Fatalf("recovery usage not folded into run accounting: got prompt=%d completion=%d, want 57/13",
			res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}
