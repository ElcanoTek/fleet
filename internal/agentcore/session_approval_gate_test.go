package agentcore

import (
	"strings"
	"testing"
)

// sentinelStager returns a fixed value from Stage to drive the gate's
// session-pre-approval branches (#300).
type sentinelStager struct{ ret string }

func (s sentinelStager) Stage(_, _, _ string) (string, error)           { return s.ret, nil }
func (s sentinelStager) StageSuggestion(string) (string, string, error) { return "", "", nil }

func TestEmailGate_SessionPreApprovedRunsNormally(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: PreApprovedSentinel})
	blocked, _ := o.checkEmailSafety("mcp_sendgrid_send_email", "call-1", `{"to":"a@b.com","content":"hi"}`)
	if blocked {
		t.Fatal("a session pre-approved send must NOT be blocked — it should run through the normal tool path")
	}
}

func TestEmailGate_SessionPreDeniedBlocks(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: PreDeniedSentinel})
	blocked, msg := o.checkEmailSafety("mcp_sendgrid_send_email", "call-1", `{"to":"a@b.com"}`)
	if !blocked || !strings.Contains(msg, "APPROVAL_DENIED") {
		t.Fatalf("pre-denied send: blocked=%v msg=%q, want blocked + APPROVAL_DENIED", blocked, msg)
	}
}

func TestEmailGate_NormalStagingUnchanged(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: "approval-xyz"})
	blocked, msg := o.checkEmailSafety("mcp_sendgrid_send_email", "call-1", `{"to":"a@b.com"}`)
	if !blocked || !strings.Contains(msg, "approval_id=approval-xyz") {
		t.Fatalf("normal staging: blocked=%v msg=%q, want APPROVAL_REQUIRED with the id", blocked, msg)
	}
}

func TestBashGate_SessionPreApprovedRunsNormally(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: PreApprovedSentinel})
	// "git push" is risky → would normally stage; the sentinel lets it run.
	blocked, _ := o.checkBashSafety("bash", "call-1", `{"command":"git push origin main"}`)
	if blocked {
		t.Fatal("a session pre-approved risky bash must NOT be blocked")
	}
}

func TestBashGate_SessionPreDeniedBlocks(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: PreDeniedSentinel})
	blocked, msg := o.checkBashSafety("bash", "call-1", `{"command":"git push origin main"}`)
	if !blocked || !strings.Contains(msg, "APPROVAL_DENIED") {
		t.Fatalf("pre-denied bash: blocked=%v msg=%q, want blocked + APPROVAL_DENIED", blocked, msg)
	}
}
