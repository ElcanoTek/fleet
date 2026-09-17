package agentcore

// A placeholder record id names no record. Replays the 2026-09-17 Reklaim
// health-scan run (task 6bd0c212): the audit declared the send with
// deal_id "n/a", the ledger bound the commitment to record "n/a", the real
// send was BLOCKED, the re-declaration stacked a second commitment that the
// send then discharged, and the stale "n/a" one wedged finish enforcement
// after the email had already gone out.

import (
	"strings"
	"testing"
)

const placeholderSendTool = "mcp_ses_outbound_send_email"

func TestIsRecordIDPlaceholder(t *testing.T) {
	for _, s := range []string{"n/a", "N/A", " n/a ", "na", "N.A.", "none", "None", "NULL", "nil", "-", "—", "not applicable", "not_applicable", "Not-Applicable"} {
		if !isRecordIDPlaceholder(s) {
			t.Errorf("%q should be a placeholder", s)
		}
		if got := normalizeDealID(s); got != "" {
			t.Errorf("normalizeDealID(%q) = %q, want empty", s, got)
		}
	}
	for _, s := range []string{"529786", "OX-1", "nab", "nano", "n/a-2", "deal-none", "0"} {
		if isRecordIDPlaceholder(s) {
			t.Errorf("%q must not be a placeholder", s)
		}
		if got := normalizeDealID(s); got != s {
			t.Errorf("normalizeDealID(%q) = %q, want unchanged", s, got)
		}
	}
	if got := declaredDealIDs([]string{" n/a ", "none", "", "77", "-"}); len(got) != 1 || got[0] != "77" {
		t.Errorf("declaredDealIDs = %v, want [77]", got)
	}
}

// The incident, end to end: a typed audit with deal_id "n/a" registers an
// UNBOUND send, the real send (which names no record) rides it and
// discharges it, and finish is allowed with nothing outstanding.
func TestTypedCommitment_PlaceholderDealIDRegistersUnbound(t *testing.T) {
	o := newOrchStateForTest()
	resp := confirmAudit(t, o, []criticalActionStruct{
		{Tool: placeholderSendTool, Identifier: "roman@elcanotek.com", DealID: "n/a"},
	}, nil)
	if resp.IsError {
		t.Fatalf("audit should pass: %s", resp.Content)
	}
	if strings.Contains(resp.Content, "(record n/a)") {
		t.Fatalf("confirm trailer must not bind the send to record n/a:\n%s", resp.Content)
	}
	if len(o.typedCommitments) != 1 || o.typedCommitments[0].hasDealBinding() {
		t.Fatalf("commitment should be unbound, got %+v", o.typedCommitments)
	}

	sendArgs := `{"to_email":"roman@elcanotek.com","subject":"Reklaim Daily Health Scan","content":"<html>…</html>"}`
	if blocked, msg := o.checkCriticalTool(placeholderSendTool, "", sendArgs); blocked {
		t.Fatalf("the declared send must be authorized, got BLOCKED: %s", msg)
	}
	o.recordToolResult(placeholderSendTool, sendArgs, `{"status_code":200,"message_id":"010f01a0af80e34c","status":"queued"}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("send did not discharge the commitment: %v", missing)
	}
	o.mu.Lock()
	o.selfAuditRequested, o.selfAuditConfirmedOnce = true, true
	o.mu.Unlock()
	if allowed, msgs := o.checkFinishEnforcement(); !allowed {
		t.Fatalf("finish must be allowed after the one declared send, got %v", msgs)
	}
}

// A placeholder on the CALL side still cannot ride a commitment bound to a
// real record: "n/a" is treated as no record, and a record-bound commitment
// fails closed on a call that names none.
func TestTypedCommitment_PlaceholderCallIDDoesNotRideRealBinding(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: typedCreateToolA, DealID: "529786"})
	for _, args := range []string{`{"deal_id":"n/a"}`, `{"deal_id":"none"}`, `{"deal_id":"-"}`} {
		if blocked, _ := o.checkCriticalTool(typedCreateToolA, "", args); !blocked {
			t.Fatalf("call %s must not ride a commitment bound to record 529786", args)
		}
	}
	// A placeholder under deal_id does not hide a real id under a sibling key.
	if got := callDealID(`{"deal_id":"n/a","internal_deal_id":"529786"}`); got != "529786" {
		t.Fatalf("callDealID = %q, want the sibling key's real id", got)
	}
	if blocked, msg := o.checkCriticalTool(typedCreateToolA, "", `{"deal_id":"n/a","internal_deal_id":"529786"}`); blocked {
		t.Fatalf("bound record under a sibling key must ride: %s", msg)
	}
}

// An all-placeholder deal_ids batch registers as one unbound action; real ids
// mixed with placeholders keep only the real ids.
func TestTypedCommitment_PlaceholderBatchIDs(t *testing.T) {
	o := newOrchStateForTest()
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealIDs: []string{"n/a", "none"}}}); got != 1 {
		t.Fatalf("all-placeholder batch registered %d units, want 1 unbound", got)
	}
	if c := o.typedCommitments[0]; c.hasDealBinding() || c.remaining != 1 {
		t.Fatalf("expected one unbound unit, got %+v", c)
	}

	o2 := newOrchStateForTest()
	if got := o2.registerCommittedActionsTyped([]criticalActionStruct{{Tool: typedCreateToolA, DealIDs: []string{"1", "n/a", "2"}}}); got != 2 {
		t.Fatalf("mixed batch registered %d units, want 2 real ids", got)
	}
	c := o2.typedCommitments[0]
	if !c.dealIDs["1"] || !c.dealIDs["2"] || c.dealIDs["n/a"] || c.dealIDs[""] {
		t.Fatalf("batch ids = %v, want exactly {1,2}", c.dealIDs)
	}
}

// Without the fix the re-declaration path stacked; with it, even the
// incident's second audit is harmless: it supersedes the (now unbound) first.
func TestTypedCommitment_PlaceholderThenReauditDoesNotStack(t *testing.T) {
	o := newOrchStateForTest()
	registerTyped(t, o, criticalActionStruct{Tool: placeholderSendTool, DealID: "n/a"})
	if got := o.registerCommittedActionsTyped([]criticalActionStruct{{Tool: placeholderSendTool}}); got != 1 {
		t.Fatalf("re-audit registered %d units, want 1", got)
	}
	if got := o.committedCriticalActions[sendEmailToolSuffix]; got != 1 {
		t.Fatalf("outstanding=%d after re-audit, want 1 (superseded, not stacked)", got)
	}
	o.recordToolResult(placeholderSendTool, `{"to_email":"a@b.c"}`, `{"status_code":200,"status":"queued"}`, true)
	if missing := o.unexecutedCommitments(); len(missing) != 0 {
		t.Fatalf("one send must clear everything, got %v", missing)
	}
}
