package agentcore

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

// TestStreamRoundResetsConsecutiveCompactionsOnCleanSuccess pins the contract
// that maxConsecutiveCompactions only trips on compactions in CONSECUTIVE
// failing rounds with no clean step between. The counter is reused across all
// rounds of a Run; before the fix it was only ever incremented, so three
// well-spaced successful compactions over a long scheduled run accumulated to
// the cap and falsely killed the run with ErrContextBudgetExhausted. A clean
// stream round must reset it.
func TestStreamRoundResetsConsecutiveCompactionsOnCleanSuccess(t *testing.T) {
	primary := &namedMockModel{
		mockModel: mockModel{
			streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				return streamStop()(nil, call)
			},
		},
		name: "primary-model",
	}
	e := newMockEngine(t, primary)
	e.resilience = resilienceConfig{maxAttempts: 0}
	// Two earlier rounds force-compacted (still below the cap of 3).
	e.consecutiveCompactions = 2

	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("test task")}

	if _, err := e.streamRoundWithResilience(
		context.Background(), orch, nil, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	); err != nil {
		t.Fatalf("expected a clean stream round, got: %v", err)
	}

	if e.consecutiveCompactions != 0 {
		t.Fatalf("consecutiveCompactions = %d, want 0 after a clean stream round", e.consecutiveCompactions)
	}
}
