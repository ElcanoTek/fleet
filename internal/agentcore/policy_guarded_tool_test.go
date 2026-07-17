package agentcore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type taskTrackerTestInput struct{}

// TestPolicyGuardedTool_RecordsNativeTaskTrackerResult drives a NATIVE tool
// (task_tracker) through the real policyGuardedTool wrapper + a ScheduledPolicy
// and asserts the scheduled finish gate blocks while tasks are pending. Before
// the fix the wrapper called BeforeToolCall but never RecordToolResult, so
// latestTaskTracker.Seen stayed false in production and the gate never fired.
// TestOrchestrationFinishEnforcementTaskTracker sets latestTaskTracker directly
// and so could not catch the dead wiring; this exercises the production path.
func TestPolicyGuardedTool_RecordsNativeTaskTrackerResult(t *testing.T) {
	pol := NewScheduledPolicy(NewLogSession(), 100, 0, 0)
	// Satisfy the audit gates so checkFinishEnforcement reaches the task-tracker
	// branch (the audit gating itself is covered by other tests).
	pol.orch.selfAuditRequested = true
	pol.orch.selfAuditConfirmedOnce = true

	if canFinish, _ := pol.CanFinish(1); !canFinish {
		t.Fatal("baseline: audit satisfied and no task_tracker recorded → finish should be allowed")
	}

	inner := fantasy.NewAgentTool(
		toolNameTaskTracker,
		"task tracker",
		func(_ context.Context, _ taskTrackerTestInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(`{"summary":{"total":2,"todo":1,"in_progress":0,"done":1}}`), nil
		},
	)
	guarded := &policyGuardedTool{inner: inner, policy: pol}

	if _, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: "{}"}); err != nil {
		t.Fatalf("guarded task_tracker run returned error: %v", err)
	}

	canFinish, msgs := pol.CanFinish(1)
	if canFinish {
		t.Fatal("finish gate should be BLOCKED after a task_tracker snapshot with pending work")
	}
	if len(msgs) == 0 {
		t.Fatal("expected an enforcement message naming the pending tasks")
	}
}

func TestPolicyGuardedTool_RecordsExactBoundedGoError(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(2048)
	detector := &fakeGuardrailDetector{}
	SetGuardrail(true, false, "observe", "prompt-injection", detector)
	t.Cleanup(func() { SetGuardrail(false, false, "off", "", nil) })
	cause := errors.New(strings.Repeat("provider transport response ", 20_000))
	inner := fantasy.NewAgentTool(
		"native_failure",
		"returns an oversized Go error",
		func(context.Context, taskTrackerTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, cause
		},
	)
	policy := &gatePolicy{}
	guarded := withModelOutputBoundary(&policyGuardedTool{inner: inner, policy: policy})

	resp, err := guarded.Run(context.Background(), fantasy.ToolCall{ID: "native-error", Input: "{}"})
	if !errors.Is(err, cause) {
		t.Fatalf("bounded error lost original cause: %v", err)
	}
	if !policy.recorded || policy.recordOK {
		t.Fatalf("policy record = recorded:%t ok:%t, want true/false", policy.recorded, policy.recordOK)
	}
	if len(resp.Content) > 2048 || policy.recordText != resp.Content || err.Error() != resp.Content {
		t.Fatalf("policy/model/error bytes drift: policy=%d response=%d error=%d", len(policy.recordText), len(resp.Content), len(err.Error()))
	}
	if !strings.Contains(resp.Content, "truncated") || !resp.IsError {
		t.Fatalf("model-visible error is not an honest bounded envelope: %.200s", resp.Content)
	}
	if detector.calls != 1 {
		t.Fatalf("governed Go error was screened %d times, want exactly once", detector.calls)
	}
}
