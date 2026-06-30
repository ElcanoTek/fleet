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

func TestScheduleTaskGate_StagesForApproval(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: "appr-sched"})
	blocked, msg := o.checkScheduleTaskSafety("schedule_task", "call-1",
		`{"name":"weekly report","prompt":"summarize the week","cron":"0 9 * * MON"}`)
	if !blocked {
		t.Fatal("schedule_task must always block (the approval card is the feature)")
	}
	if !strings.Contains(msg, "APPROVAL_REQUIRED") || !strings.Contains(msg, "appr-sched") {
		t.Fatalf("schedule_task staging msg=%q, want APPROVAL_REQUIRED with the approval id", msg)
	}
}

func TestScheduleTaskGate_UnavailableWithoutSink(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	// No approval sink wired (e.g. scheduled mode). Must NOT pass through to the
	// guarded-error Run, and must NOT loop — a clear unavailable message.
	blocked, msg := o.checkScheduleTaskSafety("schedule_task", "call-1", `{"prompt":"x"}`)
	if !blocked || !strings.Contains(msg, "SCHEDULE_TASK_UNAVAILABLE") {
		t.Fatalf("no-sink schedule_task: blocked=%v msg=%q, want blocked + SCHEDULE_TASK_UNAVAILABLE", blocked, msg)
	}
}

func TestScheduleTaskGate_IgnoresOtherTools(t *testing.T) {
	o := newOrchestrationState(NewLogSession(), 100)
	o.setApprovalSink(sentinelStager{ret: "appr-x"})
	if blocked, _ := o.checkScheduleTaskSafety("bash", "call-1", `{"command":"ls"}`); blocked {
		t.Fatal("schedule_task gate must ignore non-schedule_task tools")
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
