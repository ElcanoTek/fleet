package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// overlayCloseRecorder counts RemoteMCPOverlay.Close calls through the overlay's
// CloseScope seam — the release path a broker-owned per-turn overlay actually
// runs. It is mutex-guarded because Close can be reached from the turn goroutine
// while the test goroutine reads the count.
type overlayCloseRecorder struct {
	mu     sync.Mutex
	closes int
}

func (r *overlayCloseRecorder) closeScope(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
	return nil
}

func (r *overlayCloseRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

// TestManagerRunTurn_ClosesRemoteOverlayOnEveryExitPath is the behavioral net
// for RunTurn's overlay-close defer (#1275). The per-turn remote-MCP overlay is
// a credentialed connection to the user's OAuth-connected servers, so it must be
// released exactly once on EVERY way a turn can end — not just the happy one.
// The defer sits in RunTurn behind an `if overlay != nil` while the overlay is
// opened by openTurnRemoteOverlay, so nothing short of a test that drives whole
// turns can catch a future edit that drops it.
//
// Each subtest injects an opener returning a close-recording overlay and runs a
// real turn (mock-mode sandbox + fake LLM) down one exit path:
//
//	success   — the model answers, completedTurnResult finalizes;
//	failure   — the provider returns a fatal status, failedTurnResult finalizes;
//	cancelled — the turn's context is cancelled the moment the overlay opens,
//	            so the run loop returns a partial/cancelled result.
//
// The ordering probes matter as much as the count: the overlay must still be
// open while the turn does its terminal work (the history commit), and closed by
// the time RunTurn returns to its caller.
func TestManagerRunTurn_ClosesRemoteOverlayOnEveryExitPath(t *testing.T) {
	for _, tt := range []struct {
		name   string
		steps  []fakellm.Step
		cancel bool
	}{
		{name: "success", steps: []fakellm.Step{fakellm.TextStep("overlay turn done")}},
		// 400 is a fatal (non-retryable) provider error, so the turn fails fast
		// instead of burning the retry budget.
		{name: "failure", steps: []fakellm.Step{fakellm.StatusStep(400)}},
		{name: "cancelled", steps: []fakellm.Step{fakellm.TextStep("never reached")}, cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := fakellm.New()
			fake.SetDefault(fakellm.Scenario{Steps: tt.steps})

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			closes := &overlayCloseRecorder{}
			opened := 0
			closesAtOpen := -1
			closesAtTerminalCommit := -1

			mgr := newFakeLLMManagerWithOptions(t, fake, func(opts *ManagerOptions) {
				opts.OpenRemoteMCPOverlay = func(_ context.Context, _ string, _ map[string]bool, _ RemoteMCPSelection) (*RemoteMCPOverlay, error) {
					opened++
					closesAtOpen = closes.count()
					if tt.cancel {
						// Cancelling here — after the overlay is handed to the
						// turn, before the run loop starts — is what makes the
						// cancelled path deterministic without racing the stream.
						cancel()
					}
					// An ACTIVE broker-backed overlay: the shape production
					// hands back, and the shape Validate demands a release
					// function for.
					return &RemoteMCPOverlay{
						Broker:     inertMCPBroker{},
						Servers:    map[string]bool{"hosted_remote": true},
						CloseScope: closes.closeScope,
					}, nil
				}
			})

			res, err := mgr.RunTurn(ctx, TurnInput{
				UserMessage: "hi",
				Model:       "anthropic/claude-opus-4.8",
				// A user email is what arms the overlay at all.
				UserEmail: "user@example.test",
				CommitTerminal: func([]HistoryEntry, bool) error {
					closesAtTerminalCommit = closes.count()
					return nil
				},
			}, &recordingSink{})

			switch {
			case tt.cancel:
				if err != nil {
					t.Fatalf("cancelled turn returned err = %v, want the partial result", err)
				}
				if res == nil || !res.Cancelled {
					t.Fatalf("cancelled turn result = %+v, want Cancelled", res)
				}
			case tt.steps[0].Kind == fakellm.StepStatus:
				if err == nil {
					t.Fatal("turn against a fatal provider status returned no error")
				}
				if !errors.Is(err, ErrModelSelectionRequired) {
					t.Fatalf("failed turn err = %v, want a model-selection failure", err)
				}
			default:
				if err != nil {
					t.Fatalf("RunTurn: %v", err)
				}
				if res == nil || res.Cancelled {
					t.Fatalf("successful turn result = %+v, want a completed turn", res)
				}
			}

			if opened != 1 {
				t.Fatalf("overlay opener called %d time(s), want exactly 1", opened)
			}
			if closesAtOpen != 0 {
				t.Errorf("overlay had already been closed %d time(s) when it was opened", closesAtOpen)
			}
			// -1 means the terminal commit never ran (the failure path commits
			// nothing without committed side effects) — nothing to assert there.
			if closesAtTerminalCommit > 0 {
				t.Errorf("overlay closed before the turn committed its terminal history (closes=%d); the close must happen at RunTurn exit", closesAtTerminalCommit)
			}
			if got := closes.count(); got != 1 {
				t.Fatalf("overlay Close fired %d time(s) after RunTurn returned, want exactly 1 — a leaked per-turn overlay keeps a credentialed remote-MCP connection open", got)
			}
		})
	}
}
