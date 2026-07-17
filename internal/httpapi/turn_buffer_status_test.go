package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// turn.model_required is the engine's deliberate substitute for turn.error
// (the user can fix it by switching models) — but the turn still failed.
// Regression: inferTerminalStatus defaulted it to completed, sealing the turn
// with history_committed_at NULL and making RecoverStrandedTurns skip it.
func TestInferTerminalStatusModelRequired(t *testing.T) {
	events := []bufferedEvent{
		{Name: "turn.started"},
		{Name: "turn.model_required"},
	}
	if got := inferTerminalStatus(events); got != store.TurnStatusError {
		t.Errorf("inferTerminalStatus(model_required) = %q, want %q", got, store.TurnStatusError)
	}
	// The clean default is untouched.
	if got := inferTerminalStatus([]bufferedEvent{{Name: "turn.started"}}); got != store.TurnStatusCompleted {
		t.Errorf("inferTerminalStatus(no marker) = %q, want completed", got)
	}
}
