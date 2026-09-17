package agentcore

import (
	"strings"
	"testing"
)

// The audit guidance names the bundle's self-audit protocol only when the
// bundle ships one. Reklaim's bundle had none: the finish nudge still said
// "read protocols/self-audit.md", the fallback model looked, found nothing,
// and aborted a finished report (task 41c45dc0, 2026-09-17).
func TestAuditGuidanceNamesProtocolOnlyWhenPresent(t *testing.T) {
	const send = "mcp_ses_outbound_send_email"

	withFile := NewScheduledPolicy(NewLogSession(), 10, 0, 0)
	if allowed, msgs := withFile.orchestration().checkFinishEnforcement(); allowed || len(msgs) != 1 || !strings.Contains(msgs[0], "read protocols/self-audit.md and audit the current state") {
		t.Fatalf("default nudge should name the protocol: allowed=%v msgs=%v", allowed, msgs)
	}
	if blocked, msg := withFile.orchestration().checkCriticalTool(send, "", `{"to_email":"a@b.c"}`); !blocked || !strings.Contains(msg, "Read protocols/self-audit.md and audit the current state against every requirement of the original task, call confirm_audit(...)") {
		t.Fatalf("default BLOCKED text should name the protocol: blocked=%v msg=%q", blocked, msg)
	}

	noFile := NewScheduledPolicy(NewLogSession(), 10, 0, 0)
	noFile.SetAuditProtocolRef("")
	allowed, msgs := noFile.orchestration().checkFinishEnforcement()
	if allowed || len(msgs) != 1 {
		t.Fatalf("finish must still be gated without a protocol file: allowed=%v msgs=%v", allowed, msgs)
	}
	if strings.Contains(msgs[0], "self-audit.md") || !strings.Contains(msgs[0], "Before finishing: audit the current state against every requirement of the original task") || !strings.Contains(msgs[0], "confirm_audit") {
		t.Fatalf("nudge must ask for the audit without naming a missing file: %q", msgs[0])
	}
	blocked, msg := noFile.orchestration().checkCriticalTool(send, "", `{"to_email":"a@b.c"}`)
	if !blocked || strings.Contains(msg, "self-audit.md") || !strings.Contains(msg, "requires audit first. Audit the current state against every requirement of the original task, call confirm_audit(...), then retry") {
		t.Fatalf("BLOCKED text must not name a missing file: blocked=%v msg=%q", blocked, msg)
	}

	custom := NewInteractivePolicy(0, 0, nil, nil)
	custom.SetAuditProtocolRef("protocols/qa.md")
	if blocked, msg := custom.orchestration().checkCriticalTool(send, "", `{}`); !blocked || !strings.Contains(msg, "Read protocols/qa.md and audit the current state against every requirement of the original task, call confirm_audit") {
		t.Fatalf("custom protocol path not honored: %q", msg)
	}
	custom.orchestration().mu.Lock()
	clause := custom.orchestration().auditProtocolClause()
	custom.orchestration().mu.Unlock()
	if clause != " See protocols/qa.md." {
		t.Fatalf("clause = %q", clause)
	}
}
