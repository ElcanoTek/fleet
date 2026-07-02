package agentcore

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
)

// TestStreamRoundResetsConsecutiveCompactionsOnCleanSuccess pins one half of
// the maxConsecutiveCompactions contract: a round that completes WITHOUT
// needing a force-compaction resets the counter. The counter is reused across
// all rounds of a Run; before that reset existed it was only ever incremented,
// so three well-spaced successful compactions over a long scheduled run
// accumulated to the cap and falsely killed the run with
// ErrContextBudgetExhausted. (The other half — a round that recovered VIA a
// force-compaction KEEPS its increment, so consecutive compaction rounds can
// actually reach the cap, #598 — is pinned by
// TestStreamRound_ConsecutiveForceCompactionsTripCap.)
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

// TestStreamRound_ConsecutiveForceCompactionsTripCap is the #598 regression: the
// maxConsecutiveCompactions runaway valve must be REACHABLE. Before the fix the
// counter was reset on every clean stream — and every successful round return
// passed through that reset while every error return halted the whole run — so
// the >= cap pre-flight gate always saw 0 and ErrContextBudgetExhausted was dead
// code. Now a round that only recovered VIA a force-compaction keeps its
// increment, so a history that re-overflows the window every round accumulates
// to the cap and the next round is refused with the terminal error instead of
// burning another paid compact-and-retry cycle forever.
func TestStreamRound_ConsecutiveForceCompactionsTripCap(t *testing.T) {
	// Every ODD stream call rejects the prompt as too large and every EVEN call
	// succeeds, so each round force-compacts exactly once and then completes
	// cleanly — the runaway shape: compaction "works" but the next round's
	// history overflows again.
	calls := 0
	model := &namedMockModel{name: "ctx598-runaway"}
	model.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		calls++
		if calls%2 == 1 {
			return nil, &fantasy.ProviderError{ContextTooLargeErr: true, Message: "prompt too large"}
		}
		return streamStop()(nil, call)
	}
	e := newMockEngine(t, model) // fallback shares the slug → no swap path interferes
	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("test"))
	}
	// Long enough that forceCompactMessageHistory actually compacts (needs more
	// than head + the 20-message tail); rebuilt fresh each round, as the run loop
	// grows the history between rounds.
	longHistory := func() []fantasy.Message {
		msgs := make([]fantasy.Message, 0, 30)
		for i := 0; i < 30; i++ {
			msgs = append(msgs, fantasy.NewUserMessage("filler turn"))
		}
		return msgs
	}

	for round := 1; round <= maxConsecutiveCompactions; round++ {
		if _, err := e.streamRoundWithResilience(
			context.Background(), orch, nil, 1000,
			longHistory(), buildAgent(e.model), e.model, false, buildAgent,
		); err != nil {
			t.Fatalf("round %d should recover via forced compaction, got: %v", round, err)
		}
		if e.consecutiveCompactions != round {
			t.Fatalf("consecutiveCompactions = %d after round %d, want %d (a round that recovered VIA compaction must keep its increment)",
				e.consecutiveCompactions, round, round)
		}
	}

	// The next round is refused up front: the cap is reached, so the pre-flight
	// gate surfaces the terminal error without another paid attempt.
	callsBefore := calls
	_, err := e.streamRoundWithResilience(
		context.Background(), orch, nil, 1000,
		longHistory(), buildAgent(e.model), e.model, false, buildAgent,
	)
	if !errors.Is(err, ErrContextBudgetExhausted) {
		t.Fatalf("round %d should trip ErrContextBudgetExhausted, got: %v", maxConsecutiveCompactions+1, err)
	}
	if calls != callsBefore {
		t.Fatalf("the tripped gate must refuse BEFORE streaming; model was called %d more time(s)", calls-callsBefore)
	}
}
