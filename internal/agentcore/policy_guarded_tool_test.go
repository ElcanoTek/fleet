package agentcore

import (
	"context"
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
