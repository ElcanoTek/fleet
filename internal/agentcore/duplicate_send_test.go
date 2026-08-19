package agentcore

// Tests for the duplicate-send suppression (idempotent discharge). The
// fingerprint behind the guard is recorded exclusively on a successful send, so
// a duplicate block means the action HAS happened — it must satisfy the same
// commitment trackers a successful call would, or the run is left demanding an
// action the guard will never allow. Observed end to end before this: a
// scheduled run re-rendered its email body 110 bytes larger purely to get past
// the fingerprint, burning ~25 of its 27 minutes.

import (
	"strings"
	"testing"
)

const dupSendTool = "mcp_sendgrid_send_email"

// seedSentEmail marks rawInput as already successfully sent, exactly as
// recordToolResult's success branch would.
func seedSentEmail(o *orchestrationState, rawInput string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sentEmailFingerprints[emailDedupKey(rawInput)] = struct{}{}
	o.sendEmailSuccessCount++
}

func TestDuplicateSend_SuppressionDischargesPendingAction(t *testing.T) {
	o := newOrchStateForTest()
	input := `{"to_email":"ops@example.com","subject":"daily report","content":"<html>r1</html>"}`
	seedSentEmail(o, input)

	// The shape from the incident: after the send already succeeded, a re-audit
	// re-armed a pending commitment for the same call.
	o.mu.Lock()
	o.pendingCriticalActions = append(o.pendingCriticalActions,
		pendingCriticalAction{toolName: dupSendTool, argsHash: hashString(input)})
	o.mu.Unlock()

	blocked, msg := o.checkCriticalTool(dupSendTool, "", input)
	if !blocked {
		t.Fatal("duplicate send must stay blocked — suppression is not permission to re-send")
	}
	if !strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
		t.Fatalf("suppression response = %q, want prefix %q", msg, DuplicateSendSuppressedPrefix)
	}
	o.mu.Lock()
	pending := len(o.pendingCriticalActions)
	audited := o.selfAuditRequested
	o.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending critical actions = %d after suppression, want 0 — the demand/refuse deadlock is back", pending)
	}
	if !audited {
		t.Fatal("selfAuditRequested should be set once the pending list drains, mirroring the success path")
	}
}

func TestDuplicateSend_SuppressionDischargesCommittedActionAndConsumesAudit(t *testing.T) {
	o := newOrchStateForTest()
	input := `{"to_email":"ops@example.com","subject":"daily report","content":"<html>r2</html>"}`
	seedSentEmail(o, input)

	o.registerCommittedActions([]string{"send_email: status report to ops"})
	o.mu.Lock()
	o.auditConfirmed = true
	o.mu.Unlock()

	blocked, msg := o.checkCriticalTool(dupSendTool, "", input)
	if !blocked || !strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
		t.Fatalf("blocked=%v msg=%q, want suppression", blocked, msg)
	}
	o.mu.Lock()
	missing := o.unexecutedCommitments()
	consumed := !o.auditConfirmed
	o.mu.Unlock()
	if len(missing) != 0 {
		t.Fatalf("unexecuted commitments = %v after suppression, want none", missing)
	}
	if !consumed {
		t.Fatal("audit token should be consumed once all commitments are exhausted, mirroring the success path")
	}
}

func TestDuplicateSend_FreshPayloadStillExecutes(t *testing.T) {
	o := newOrchStateForTest()
	seedSentEmail(o, `{"to_email":"ops@example.com","subject":"daily report","content":"<html>r3</html>"}`)

	// A genuinely different payload is not a duplicate; it proceeds to the
	// ordinary audit gating below the guard (blocked here only because the
	// audit has not been confirmed — the message proves which gate fired).
	blocked, msg := o.checkCriticalTool(dupSendTool, "",
		`{"to_email":"ops@example.com","subject":"daily report","content":"<html>r3 corrected</html>"}`)
	if blocked && strings.HasPrefix(msg, DuplicateSendSuppressedPrefix) {
		t.Fatalf("fresh payload wrongly suppressed as duplicate: %q", msg)
	}
}
