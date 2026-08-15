package agentcore

import "testing"

// The delegated (sub-agent) finish gate (#1043 follow-up). A child is governed
// by the SAME ScheduledPolicy as a root run; the one relaxation is that
// CanFinish does not demand the self-audit ritual, which is the top-level run's
// deliverable gate. Everything else must still hold — these tests pin both
// halves so a future change cannot quietly widen the relaxation.

// TestDelegatedPolicy_FinishesWithoutSelfAuditRitual is the fix itself: a
// scheduled root run is blocked until it confirms its audit, a delegated run is
// not. Before this, every spawned child spent rounds being told to "read
// protocols/self-audit.md" before it was allowed to answer its parent.
func TestDelegatedPolicy_FinishesWithoutSelfAuditRitual(t *testing.T) {
	root := NewScheduledPolicy(nil, 10, 0, 0)
	if ok, msgs := root.CanFinish(0); ok {
		t.Fatalf("a ROOT scheduled run must still be gated on the self-audit ritual; got ok=%v msgs=%v", ok, msgs)
	}

	child := NewDelegatedPolicy(nil, 10, 0, 0)
	ok, msgs := child.CanFinish(0)
	if !ok {
		t.Fatalf("a delegated (sub-agent) run must finish without the self-audit ritual; got msgs=%v", msgs)
	}
	if len(msgs) != 0 {
		t.Fatalf("delegated finish must carry no enforcement messages, got %v", msgs)
	}
}

// TestDelegatedPolicy_KeepsEveryOtherFinishGate pins the narrowness of the
// relaxation: a child with pending task-tracker work is still refused a finish,
// exactly like a root run. Only the audit RITUAL is skipped.
func TestDelegatedPolicy_KeepsEveryOtherFinishGate(t *testing.T) {
	child := NewDelegatedPolicy(nil, 10, 0, 0)
	child.orchestration().latestTaskTracker = taskTrackerSnapshot{Seen: true, Todo: 2}

	if ok, msgs := child.CanFinish(0); ok {
		t.Fatalf("a delegated run with unfinished tracker items must NOT finish; got ok=%v msgs=%v", ok, msgs)
	}

	child.orchestration().latestTaskTracker = taskTrackerSnapshot{Seen: true}
	if ok, msgs := child.CanFinish(0); !ok {
		t.Fatalf("delegated run with a clear tracker must finish; msgs=%v", msgs)
	}
}

// TestDelegatedPolicy_SharesTheScheduledGateChain proves the delegated policy is
// the same governed bundle — same type, same BeforeToolCall chain — so a child's
// ceilings and critical-tool gating are not a second, weaker path.
func TestDelegatedPolicy_SharesTheScheduledGateChain(t *testing.T) {
	child := NewDelegatedPolicy(nil, 10, 0.10, 0)
	// Ceiling gate: charge past the cost ceiling, then the next tool call is
	// blocked exactly as it would be in a root run.
	child.ChargeChildUsage(RunUsage{CostUSD: 0.20})
	blocked, msg := child.BeforeToolCall("bash", "call-1", `{"command":"ls"}`)
	if !blocked {
		t.Fatal("delegated policy must still enforce the cost ceiling before a tool call")
	}
	if msg == "" {
		t.Fatal("a blocked call must explain itself to the model")
	}
}
