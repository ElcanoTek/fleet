package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/config"
)

// newTestScheduledAgent builds a scheduled Agent over a mock model with no MCP
// servers and no captain's log (so Execute touches no network / git).
func newTestScheduledAgent(t *testing.T, model fantasy.LanguageModel) *Agent {
	t.Helper()
	t.Setenv("FLEET_LOG_FILE", t.TempDir()+"/session.json")
	return NewAgent(Options{
		Config:        &config.Config{MaxIterations: 50, LLMMaxTokens: 4096, MCPServers: map[string]config.MCPServerConfig{}},
		Model:         model,
		SystemPrompt:  "you are a scheduled agent",
		MaxIterations: 50,
	})
}

// TestExecute_NilModelReturnsError pins the no-model guard.
func TestExecute_NilModelReturnsError(t *testing.T) {
	a := newTestScheduledAgent(t, nil)
	a.model = nil
	if err := a.Execute(context.Background(), "do the thing"); err == nil {
		t.Fatal("expected error with no model configured")
	}
}

// TestExecute_ScheduledDoesNotCollapseToOneRound verifies the scheduled driver
// engages the FULL enforcement loop (Mode=Scheduled) rather than the interactive
// 1-round collapse: a model that just stops without ever calling confirm_audit
// never satisfies finish enforcement, so the loop keeps injecting nudges and
// streams more than once before bounding out at the max-rounds cap. This is the
// observable difference from the interactive InteractivePolicy (which finishes
// at round 0). The terminal error is expected — the point is that the scheduled
// Policy.CanFinish blocked finishing.
func TestExecute_ScheduledDoesNotCollapseToOneRound(t *testing.T) {
	streams := int32(0)
	model := &itMockModel{
		streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
			atomic.AddInt32(&streams, 1)
			return func(yield func(fantasy.StreamPart) bool) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}, nil
		},
	}
	a := newTestScheduledAgent(t, model)

	err := a.Execute(context.Background(), "complete the task")
	// The audit never clears, so the loop exhausts the round cap and errors.
	if err == nil {
		t.Fatal("expected max-rounds error when audit never clears")
	}
	if got := atomic.LoadInt32(&streams); got < 2 {
		t.Errorf("scheduled run must NOT collapse to 1 round; streamed %d times", got)
	}
}
