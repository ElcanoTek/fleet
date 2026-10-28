package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// TestManagerRunTurn_FailedAfterToolsPersistsFailureSummary pins the record a
// turn leaves when its provider fails after a tool has run (ADR-0035): the
// executed records commit, and they close with a turn_summary that says the
// turn FAILED. Without that summary a reload showed a run of tool calls with
// no answer, which the web reads as a model that stopped on its own — "The
// assistant finished without a written reply." over a failed turn.
//
// The tool call fails (an unknown tool), which is what keeps the provider
// failure from resuming at the completed step (ADR-0065) and sends the turn
// down the committed-side-effects path.
func TestManagerRunTurn_FailedAfterToolsPersistsFailureSummary(t *testing.T) {
	t.Setenv("FLEET_RETRY_MAX_ATTEMPTS", "0")
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		{Kind: fakellm.StepToolCalls, ToolCalls: []fakellm.ToolCall{{ID: "call_1", Name: "no_such_tool", Arguments: `{}`}}},
		fakellm.StatusStep(500),
	}})
	mgr := newFakeLLMManager(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const model = "anthropic/claude-opus-4.8"
	var committed []HistoryEntry
	sink := &recordingSink{}
	_, err := mgr.RunTurn(ctx, TurnInput{
		UserMessage: "build the page",
		Model:       model,
		CommitTerminal: func(entries []HistoryEntry, _ bool) error {
			committed = append(committed, entries...)
			return nil
		},
	}, sink)
	if !errors.Is(err, ErrModelSelectionRequired) {
		t.Fatalf("RunTurn err = %v, want a model-selection failure", err)
	}
	if !sink.has("turn.model_required") {
		t.Fatalf("missing turn.model_required; saw %v", sink.names())
	}

	var sawResult bool
	for _, e := range committed {
		if e.Type == "tool_result" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("committed %d entries without the executed tool_result: %+v", len(committed), committed)
	}
	last := committed[len(committed)-1]
	if last.Type != "turn_summary" {
		t.Fatalf("last committed entry type = %q, want turn_summary", last.Type)
	}
	var summary TurnSummaryContent
	if err := json.Unmarshal(last.Content, &summary); err != nil {
		t.Fatalf("decode turn_summary: %v", err)
	}
	if summary.Cancelled {
		t.Error("failed turn's summary says cancelled")
	}
	if summary.Failure == nil {
		t.Fatalf("turn_summary has no failure: %s", last.Content)
	}
	if summary.Failure.Reason == "" || summary.Failure.Message == "" || summary.Failure.FailedModel != model {
		t.Errorf("failure = %+v, want a reason, a message and failed_model %q", *summary.Failure, model)
	}
	if summary.Model != model {
		t.Errorf("summary model = %q, want %q", summary.Model, model)
	}

	// The persisted failure is the card the live stream carried.
	var live map[string]any
	for _, ev := range sink.events {
		if ev.name == "turn.model_required" {
			live, _ = ev.payload.(map[string]any)
		}
	}
	if live["reason"] != summary.Failure.Reason || live["message"] != summary.Failure.Message {
		t.Errorf("persisted failure %+v differs from the live card %v", *summary.Failure, live)
	}
}
