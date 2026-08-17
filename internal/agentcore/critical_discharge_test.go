package agentcore

import (
	"strings"
	"testing"
)

// Reproduces the wedge from a real scheduled run (Comfluence daily refresh,
// fleet-task-3f8e57b7): a critical tool is blocked pre-audit, the retry fails
// tool-argument validation, and the call that finally succeeds therefore carries
// CORRECTED arguments. Discharging on (toolName, argsHash) alone never matched
// that success, so the run was told to "Execute pending action(s)" after it had
// already sent the email — and went on to defeat the duplicate-send guard by
// padding the payload.
func TestPendingCriticalDischargedByCorrectedArguments(t *testing.T) {
	const tool = "mcp_sendgrid_send_email"
	// The three payloads from that run: blocked, rejected, accepted.
	const blocked = `{"to":["roman@elcanotek.com"],"subject":"s","content":"<html>1</html>"}`
	const rejected = `{"to_email":["roman@elcanotek.com"],"subject":"s","content":"<html>1</html>"}`
	const accepted = `{"to_email":"roman@elcanotek.com","subject":"s","content":"<html>1</html>"}`

	o := newOrchestrationState(nil, 0)

	// 1. Pre-audit call is blocked and recorded as an outstanding commitment.
	if isBlocked, _ := o.checkCriticalTool(tool, "", blocked); !isBlocked {
		t.Fatal("a critical tool before confirm_audit must be blocked")
	}
	if got := len(o.pendingCriticalActions); got != 1 {
		t.Fatalf("pendingCriticalActions = %d, want 1", got)
	}

	// 2. Audit clears, so the tool may now run.
	o.mu.Lock()
	o.auditConfirmed = true
	o.selfAuditRequested = true
	o.selfAuditConfirmedOnce = true
	o.mu.Unlock()

	// 3. The retry is rejected for bad arguments — a failure discharges nothing.
	o.recordToolResult(tool, rejected, `{"error":"to_email: Input should be a valid string"}`, false)
	if got := len(o.pendingCriticalActions); got != 1 {
		t.Fatalf("a FAILED retry discharged the commitment: pending = %d, want 1", got)
	}

	// 4. The corrected call succeeds. Its args differ from the blocked call's,
	//    because fixing them is precisely what made it succeed.
	o.recordToolResult(tool, accepted, `{"status_code":202,"status":"queued","message_id":"x"}`, true)
	if got := len(o.pendingCriticalActions); got != 0 {
		t.Fatalf("successful send left %d pending action(s); the run would be told to send again", got)
	}
	if len(o.completedCriticalActions) != 1 {
		t.Fatalf("completedCriticalActions = %v, want one entry", o.completedCriticalActions)
	}
}

// The precise entry is still preferred when the retry really is the same call,
// so a same-tool pair is discharged one-for-one rather than collapsing.
func TestPendingCriticalPrefersExactArgsMatch(t *testing.T) {
	const tool = "mcp_sendgrid_send_email"
	const first = `{"to_email":"a@example.com","subject":"one"}`
	const second = `{"to_email":"b@example.com","subject":"two"}`

	o := newOrchestrationState(nil, 0)
	for _, args := range []string{first, second} {
		if isBlocked, _ := o.checkCriticalTool(tool, "", args); !isBlocked {
			t.Fatal("expected pre-audit block")
		}
	}
	if got := len(o.pendingCriticalActions); got != 2 {
		t.Fatalf("pending = %d, want 2 distinct commitments", got)
	}
	o.mu.Lock()
	o.auditConfirmed = true
	o.mu.Unlock()

	// Succeeding the SECOND payload must discharge that one, not the first.
	o.recordToolResult(tool, second, `{"status_code":202,"status":"queued"}`, true)
	if got := len(o.pendingCriticalActions); got != 1 {
		t.Fatalf("pending = %d, want 1 after one success", got)
	}
	o.mu.Lock()
	remaining := o.pendingCriticalActions[0].argsHash
	o.mu.Unlock()
	if remaining != hashString(first) {
		t.Error("the exact-args hit discharged the wrong commitment")
	}

	// And two commitments still need two successes.
	o.recordToolResult(tool, first, `{"status_code":202,"status":"queued"}`, true)
	if got := len(o.pendingCriticalActions); got != 0 {
		t.Fatalf("pending = %d, want 0 after both succeeded", got)
	}
}

// A success for one tool must not discharge another tool's commitment.
func TestPendingCriticalDoesNotCrossTools(t *testing.T) {
	const email = "mcp_sendgrid_send_email"
	const template = "mcp_sendgrid_send_template_email"

	o := newOrchestrationState(nil, 0)
	if isBlocked, _ := o.checkCriticalTool(email, "", `{"to_email":"a@example.com"}`); !isBlocked {
		t.Fatal("expected pre-audit block")
	}
	if isBlocked, _ := o.checkCriticalTool(template, "", `{"to_email":"a@example.com"}`); !isBlocked {
		t.Fatal("expected pre-audit block")
	}
	o.mu.Lock()
	o.auditConfirmed = true
	o.mu.Unlock()

	o.recordToolResult(email, `{"to_email":"corrected@example.com"}`, `{"status_code":202,"status":"queued"}`, true)
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.pendingCriticalActions) != 1 {
		t.Fatalf("pending = %d, want 1 — the other tool's commitment must survive", len(o.pendingCriticalActions))
	}
	if o.pendingCriticalActions[0].toolName != template {
		t.Errorf("surviving commitment = %q, want %q", o.pendingCriticalActions[0].toolName, template)
	}
}

// End to end through the finish gate: after the corrected send succeeds,
// CanFinish must stop demanding the action. This is the message the wedged run
// saw three times.
func TestCanFinishStopsDemandingAfterCorrectedSend(t *testing.T) {
	const tool = "mcp_sendgrid_send_email"
	o := newOrchestrationState(nil, 0)

	if isBlocked, _ := o.checkCriticalTool(tool, "", `{"to":["roman@elcanotek.com"]}`); !isBlocked {
		t.Fatal("expected pre-audit block")
	}
	o.mu.Lock()
	o.auditConfirmed = true
	o.selfAuditRequested = true
	o.selfAuditConfirmedOnce = true
	o.mu.Unlock()

	if ok, msgs := o.checkFinishEnforcement(); ok {
		t.Fatal("must not finish while the commitment is outstanding")
	} else if len(msgs) == 0 || !strings.Contains(msgs[0], "pending action") {
		t.Fatalf("unexpected refusal: %v", msgs)
	}

	o.recordToolResult(tool, `{"to_email":"roman@elcanotek.com"}`, `{"status_code":202,"status":"queued"}`, true)

	if ok, msgs := o.checkFinishEnforcement(); !ok {
		t.Fatalf("still refusing to finish after a successful send: %v", msgs)
	}
}
