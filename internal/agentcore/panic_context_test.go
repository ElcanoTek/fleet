package agentcore

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// ctxRecorder is a panic handler that records the run correlation of the
// context each event was emitted under.
type ctxRecorder struct {
	mu   sync.Mutex
	runs []string
}

func (h *ctxRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *ctxRecorder) Handle(ctx context.Context, _ slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, logging.Value(ctx, logging.KeyRunID)+"/"+logging.Value(ctx, logging.KeyTurnID))
	return nil
}
func (h *ctxRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *ctxRecorder) WithGroup(string) slog.Handler      { return h }

type panickingFinishPolicy struct{ passPolicy }

func (panickingFinishPolicy) CanFinish(int) (bool, []string) { panic("finish gate raw panic") }

// A panic contained at a run boundary is emitted under the run's context, so
// the event carries run_id and turn_id — the tool boundary under the tool
// call's context, a boundary with no context of its own (the finish gate)
// under the run's (#2074 review: every boundary emitted under a background
// context).
func TestContainedPanicCarriesTheRunContext(t *testing.T) {
	rec := &ctxRecorder{}
	safe.SetPanicHandler(rec)
	t.Cleanup(func() { safe.SetPanicHandler(nil) })

	runCtx := logging.WithTurn(logging.WithTask(context.Background(), "task-1", "run-42"), "conv-1", "turn-7")
	attribution := panicAttribution{runMode: ModeScheduled.String(), taskID: "task-1", ctx: runCtx}

	tool := containTool(newPanickingTool("boom", "raw tool panic"), attribution, nil)
	callCtx, cancel := context.WithCancel(runCtx) // what the model loop hands a tool
	defer cancel()
	if _, err := tool.Run(callCtx, fantasy.ToolCall{ID: "call-1", Input: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := callPolicyCanFinish(panickingFinishPolicy{}, 1, "done", attribution); err == nil {
		t.Fatal("the finish-gate panic was not contained as an error")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.runs) != 2 {
		t.Fatalf("events = %v", rec.runs)
	}
	for _, r := range rec.runs {
		if r != "run-42/turn-7" {
			t.Fatalf("a contained panic lost the run context: %v", rec.runs)
		}
	}
}
