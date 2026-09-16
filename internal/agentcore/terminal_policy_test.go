package agentcore

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
)

type terminalTestPolicy struct {
	passPolicy
	failure error
}

func (p terminalTestPolicy) TerminalError() error { return p.failure }

// A terminal failure overrides even CanFinish=true and never enters the
// structured-output completion phase or another paid model turn.
func TestTerminalPolicyStopsWithPartialWork(t *testing.T) {
	calls := 0
	model := &mockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "partial work"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 3}})
		}, nil
	}}
	res, err := Run(context.Background(), ModeScheduled, RunConfig{
		OutputSchema: []byte(`{"type":"object","properties":{"done":{"type":"boolean"}},"required":["done"],"additionalProperties":false}`),
	}, Deps{
		Input:  stubInput{system: "sys", user: "finish", label: "finish"},
		Policy: terminalTestPolicy{failure: ErrCompletionUnverified}, Model: model,
	})
	if !errors.Is(err, ErrCompletionUnverified) || calls != 1 || res.Cancelled || res.AuditAborted {
		t.Fatalf("terminal outcome: err=%v calls=%d result=%+v", err, calls, res)
	}
	if res.FinalText != "partial work" || len(res.Entries) == 0 || res.Usage.PromptTokens != 10 || res.Rounds != 1 || len(res.OutputJSON) != 0 {
		t.Fatalf("lost partial work or emitted successful structured output: %+v", res)
	}
}

type terminalPanicPolicy struct{ passPolicy }

func (terminalPanicPolicy) TerminalError() error { panic("terminal raw panic") }

func TestTerminalPolicyPanicIsContained(t *testing.T) {
	collector := capturePanicEvents(t)
	_, err := Run(context.Background(), ModeScheduled, RunConfig{}, Deps{
		Input:  stubInput{system: "sys", user: "finish", label: "finish"},
		Policy: terminalPanicPolicy{}, Model: &mockModel{streamFunc: streamStop()},
	})
	if !errors.Is(err, ErrRunBoundaryPanic) || len(collector.snapshot()) != 1 {
		t.Fatalf("uncontained terminal policy panic: %v", err)
	}
}
