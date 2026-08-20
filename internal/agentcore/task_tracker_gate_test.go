package agentcore

// #1125: pin the task_tracker tool-output → scheduled finish-gate coupling to
// the REAL tool's output function. Every input below is built by running
// internal/tools' task_tracker (or by passing its output through the exact
// production reshaping — the post-hook fragment join and the truncation
// envelope), never from a hand-written fixture, so a change to the tool's
// output format breaks these tests instead of silently disabling the
// pending-work finish gate (checkFinishEnforcement).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// Long-ish titles on purpose: the resulting document must exceed the
// model-output boundary's minimum ceiling (MinMaxToolOutputBytes) so the
// envelope shape below is produced by the REAL boundary, not simulated.
const pendingTaskListJSON = `[
	{"id":"1","title":"download the quarterly spend report from the reporting bucket and stage it in the conversation workspace for processing","status":"done"},
	{"id":"2","title":"compute the per-client rollup with the shared aggregation notebook and sanity-check the totals against last quarter","status":"in_progress"},
	{"id":"3","title":"email the finished summary to the whole distribution list with the rollup spreadsheet and methodology notes attached","status":"todo"}]`

// runRealTracker drives the real task_tracker tool and returns its raw
// response content.
func runRealTracker(t *testing.T, tracker fantasy.AgentTool, callID, input string) string {
	t.Helper()
	resp, err := tracker.Run(context.Background(), fantasy.ToolCall{ID: callID, Input: input})
	if err != nil {
		t.Fatalf("task_tracker run: %v", err)
	}
	if resp.IsError {
		t.Fatalf("task_tracker errored: %s", resp.Content)
	}
	return resp.Content
}

func TestParseTaskTrackerSnapshot_RealToolOutputShapes(t *testing.T) {
	raw := runRealTracker(t, tools.NewTaskTrackerTool(), "tc-1",
		`{"command":"plan","task_list":`+pendingTaskListJSON+`}`)

	want := taskTrackerSnapshot{Seen: true, Total: 3, Todo: 1, InProgress: 1, Done: 1}
	assertSnap := func(shape, input string) {
		t.Helper()
		if got := parseTaskTrackerSnapshot(input); got != want {
			t.Fatalf("%s: snapshot = %+v, want %+v\ninput: %.300s", shape, got, want, input)
		}
	}

	// The tool's own JSON document (the structured fast path).
	assertSnap("plain JSON", raw)

	// A post_tool_use hook fragment appended after the JSON — the exact bytes
	// recordToolResult receives when a hook adds context (appendHookContext):
	// the result no longer parses as one JSON document, which used to return
	// Seen=false and disarm the gate.
	assertSnap("hook fragment appended", appendHookContext(raw, "post-hook: lint clean"))

	// The tool's own human rendering, extracted from the real document rather
	// than hand-written — the shape renderResult falls back to when its JSON
	// marshal fails.
	var doc struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || !strings.Contains(doc.Output, "Summary:") {
		t.Fatalf("tracker output field lost its Summary line: err=%v output=%q", err, doc.Output)
	}
	assertSnap("human summary line", doc.Output)

	// The context reducer's truncation envelope, quoting the original head as
	// its preview (tool_context_budget.go, aggregateResultEnvelope).
	assertSnap("context-reducer envelope", aggregateResultEnvelope(toolNameTaskTracker, raw, 4096))

	// The model-output boundary's envelope (tool_output_limit.go) — the OTHER
	// production truncation shape, and the one recordToolResult actually
	// receives when the tool's output exceeds the model-visible ceiling.
	// Produced by the real boundary under a lowered admin ceiling.
	SetMaxToolOutputBytes(MinMaxToolOutputBytes)
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	if len(raw) <= MinMaxToolOutputBytes {
		t.Fatalf("test shape broken: raw tracker output is %d bytes and must exceed the %d-byte ceiling to be enveloped — lengthen pendingTaskListJSON's titles", len(raw), MinMaxToolOutputBytes)
	}
	bounded := boundModelVisibleToolResponse(context.Background(), toolNameTaskTracker, "tc-1", fantasy.NewTextResponse(raw))
	assertSnap("model-output boundary envelope", bounded.Content)
}

func TestParseTaskTrackerSnapshot_EmptyTrackerStaysUnseen(t *testing.T) {
	raw := runRealTracker(t, tools.NewTaskTrackerTool(), "tc-0", `{"command":"view"}`)
	if got := parseTaskTrackerSnapshot(raw); got.Seen {
		t.Fatalf("an empty tracker must not arm the finish gate: %+v", got)
	}
}

// TestTaskTrackerFinishGate_CouplesToRealToolOutput drives the real tool
// through the production policyGuardedTool wrapper and asserts the scheduled
// finish gate arms on pending work and releases when the SAME tool reports
// everything done.
func TestTaskTrackerFinishGate_CouplesToRealToolOutput(t *testing.T) {
	pol := NewScheduledPolicy(NewLogSession(), 100, 0, 0)
	// Satisfy the audit gates so checkFinishEnforcement reaches the
	// task-tracker branch (audit gating is covered elsewhere).
	pol.orch.selfAuditRequested = true
	pol.orch.selfAuditConfirmedOnce = true

	guarded := &policyGuardedTool{inner: tools.NewTaskTrackerTool(), policy: pol}
	if _, err := guarded.Run(context.Background(), fantasy.ToolCall{
		ID: "tc-1", Input: `{"command":"plan","task_list":` + pendingTaskListJSON + `}`,
	}); err != nil {
		t.Fatalf("guarded task_tracker run: %v", err)
	}
	if canFinish, msgs := pol.CanFinish(1); canFinish || len(msgs) == 0 {
		t.Fatalf("finish gate must block on the real tool's pending work: canFinish=%t msgs=%v", canFinish, msgs)
	}

	allDone := `[
		{"id":"1","title":"download the report","status":"done"},
		{"id":"2","title":"compute the rollup","status":"done"},
		{"id":"3","title":"email the summary","status":"done"}]`
	if _, err := guarded.Run(context.Background(), fantasy.ToolCall{
		ID: "tc-2", Input: `{"command":"plan","task_list":` + allDone + `}`,
	}); err != nil {
		t.Fatalf("guarded task_tracker run: %v", err)
	}
	if canFinish, msgs := pol.CanFinish(2); !canFinish {
		t.Fatalf("finish gate must release once the real tool reports all done: %v", msgs)
	}
}
