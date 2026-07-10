package agentcore

// Pins the shared-loop cache wiring end-to-end: roundState.stream installs
// promptCachingStep WITH the compaction-summary breakpoint, so a compaction
// summary sitting at the head of the tail reaches the provider carrying its
// own cache_control marker (a stable boundary between the cached head and the
// evolving tail), alongside the last-system + last-two-window markers. Before
// this wiring the option existed (and interactive.go tagged summaries for it)
// but no production caller ever enabled it.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
)

func TestStreamRound_CompactionSummaryBreakpointWired(t *testing.T) {
	summaryText := compactionSummaryPrefix + "] 12 earlier messages condensed."

	var markedRoles []fantasy.MessageRole
	summaryMarked := false
	model := &namedMockModel{name: "anthropic/claude-sonnet-4.6"}
	model.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		for _, msg := range call.Prompt {
			if _, ok := msg.ProviderOptions[anthropic.Name]; !ok {
				continue
			}
			markedRoles = append(markedRoles, msg.Role)
			if isCompactionSummaryMessage(msg) {
				summaryMarked = true
			}
		}
		return streamStop()(nil, call)
	}

	e := newMockEngine(t, model)
	orch := newOrchestrationState(e.logSession, 50)
	buildAgent := func(m fantasy.LanguageModel) fantasy.Agent {
		return fantasy.NewAgent(m, fantasy.WithSystemPrompt("system prompt"))
	}
	// Summary at the head of the tail, followed by enough turns that the
	// last-two rolling window cannot reach it — only the dedicated breakpoint
	// can mark it.
	messages := []fantasy.Message{
		fantasy.NewUserMessage(summaryText),
		fantasy.NewUserMessage("first follow-up"),
		fantasy.NewUserMessage("second follow-up"),
		fantasy.NewUserMessage("third follow-up"),
	}

	if _, err := e.streamRoundWithResilience(
		context.Background(), orch, nil, 1000,
		messages, buildAgent(e.model), e.model, false, buildAgent,
	); err != nil {
		t.Fatalf("stream round failed: %v", err)
	}

	if !summaryMarked {
		t.Error("compaction summary reached the provider without a cache_control marker — WithCompactionSummaryBreakpoint is not wired into the shared loop")
	}
	// system + summary + last-two window = 4, Anthropic's per-request maximum.
	if len(markedRoles) != 4 {
		t.Errorf("marked messages = %d (%v), want 4 (system + summary + last two)", len(markedRoles), markedRoles)
	}
}
