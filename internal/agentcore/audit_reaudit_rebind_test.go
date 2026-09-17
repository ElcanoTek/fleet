// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"strings"
	"testing"
)

// Reklaim 6bd0c212 (#1535): the audit bound a send to a record it could never
// carry, the send was BLOCKED and told to re-audit, the re-audit named a
// different record set, and the same-shape supersede rule left BOTH on the
// ledger. The send discharged the fresh one; the stale one wedged finish
// enforcement until the model aborted a run whose email had gone out. A
// re-audit of the same tool now supersedes a prior declaration nothing has
// executed under, whatever its binding.
func TestReauditOfSameToolWithNewBindingSupersedesNeverExecutedCommitment(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-WRONG"})

	// The real call targets a different record: blocked, steered to re-audit.
	blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal_id":"OX-1"}`)
	if !blocked || !strings.Contains(msg, "matches no outstanding audited commitment") {
		t.Fatalf("mismatched record must be blocked with the re-audit guidance, got blocked=%v msg=%s", blocked, msg)
	}
	if !strings.Contains(msg, "supersedes the declaration that refused it") {
		t.Errorf("BLOCKED guidance should tell the model a re-audit retires the stale binding, got: %s", msg)
	}

	// Re-audit the SAME tool bound to the correct record: the never-executed
	// wrong-record declaration is superseded instead of stacking.
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealID: "OX-1"}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	o.auditConfirmed = true
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("stale wrong-record commitment stacked: outstanding=%d, want 1", got)
	}
	o.mu.Lock()
	outstanding := o.outstandingCommitmentSummary()
	o.mu.Unlock()
	if len(outstanding) != 1 || !strings.Contains(outstanding[0], "(record OX-1)") || strings.Contains(strings.Join(outstanding, " "), "OX-WRONG") {
		t.Fatalf("only the corrected commitment may remain outstanding, got %v", outstanding)
	}

	// The corrected call rides, discharges, and finish is allowed.
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal_id":"OX-1"}`); blocked {
		t.Fatalf("corrected call must be authorized, got blocked: %s", msg)
	}
	o.recordToolResult(typedCreateToolA, `{"deal_id":"OX-1"}`, `{"deal_id":"OX-1","ok":true}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("expected nothing outstanding after the corrected call, got %v", missing)
	}
	o.mu.Lock()
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	o.mu.Unlock()
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish must be allowed — the only real obligation executed, got %v", msgs)
	}
}

// The new shape never touches a commitment that HAS authorized work: a
// partially executed batch keeps its remaining records when the model
// re-audits the same tool for a different record, exactly as before.
func TestReauditDoesNotRetireACommitmentSomethingExecutedUnder(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealIDs: []string{"OX-A", "OX-B"}})
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal_id":"OX-A"}`); blocked {
		t.Fatalf("approved record must be authorized, got blocked: %s", msg)
	}
	o.recordToolResult(typedCreateToolA, `{"deal_id":"OX-A"}`, `{"deal_id":"OX-A","ok":true}`, true)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 1 {
		t.Fatalf("one record discharged should leave 1 outstanding, got %d", got)
	}

	// Re-audit the same tool for an unrelated record: OX-B stays owed.
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealID: "OX-C"}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("a partially executed batch must not be retired by a re-bound re-audit: outstanding=%d, want 2 (OX-B + OX-C)", got)
	}
	o.mu.Lock()
	outstanding := strings.Join(o.outstandingCommitmentSummary(), " | ")
	o.mu.Unlock()
	if !strings.Contains(outstanding, "OX-B") || !strings.Contains(outstanding, "OX-C") {
		t.Fatalf("both OX-B and OX-C must remain owed, got %s", outstanding)
	}
}

// Several same-tool declarations inside ONE audit are siblings, not
// re-audits: the preExisting snapshot still protects them from each other.
func TestSameEnvelopeSameToolDeclarationsStillCoexist(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o,
		criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-1"},
		criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-2"},
	)
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("two sibling declarations in one audit must both register, got %d", got)
	}
}

// The carve-out is exactly the correction loop: without a BLOCKED attempt that
// the earlier declaration refused, a re-audit of the same tool for a different
// record is a second obligation and both stay owed — retiring the first would
// let the run finish without it.
func TestReauditWithNewBindingButNoRefusedAttemptStacks(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-77"})
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealID: "OX-5"}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	if got := o.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("a pending record-bound obligation no call collided with must survive a re-bound re-audit: outstanding=%d, want 2", got)
	}

	// A refusal for a DIFFERENT record than the one re-declared does not
	// qualify either: the model tried OX-9, then declared OX-5.
	o2 := newOrchStateForTest()
	registerTyped(t, o2, criticalActionStruct{Tool: typedCreateToolA, DealID: "OX-77"})
	if blocked, _ := o2.checkCriticalTool(typedCreateToolA, "", `{"deal_id":"OX-9"}`); !blocked {
		t.Fatal("OX-9 must be blocked against an OX-77 binding")
	}
	o2.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealID: "OX-5"}})
	if got := o2.committedCriticalActions["create_prepared_deal"]; got != 2 {
		t.Fatalf("re-declaring a record the refused call did not carry must stack, outstanding=%d, want 2", got)
	}
}
