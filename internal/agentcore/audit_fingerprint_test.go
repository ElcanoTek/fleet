package agentcore

import (
	"strings"
	"testing"
)

// The "Audit already confirmed" shortcut answers an identical re-audit
// without re-granting the token. Its fingerprint is recorded when the token
// is granted, before the critical call runs, so a failed call has to clear
// it: otherwise the re-audit the model makes to retry is told to finish, the
// token (consumed by the failure) stays spent, and the action is never done.
func TestConfirmAudit_IdenticalReauditAfterFailedCallRegrantsToken(t *testing.T) {
	const tool = "mcp_ses_send_email"
	const input = `{"to_email":"ops@example.com","subject":"weekly report","content":"<p>numbers</p>"}`
	legacy := []string{"none"} // declares nothing a suffix matches: no commitment, one-shot token

	o := newOrchStateForTest()
	if resp := confirmAudit(t, o, nil, legacy); resp.IsError {
		t.Fatalf("first audit refused: %q", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(tool, "", input); blocked {
		t.Fatalf("audited call refused: %q", msg)
	}
	o.recordToolResult(tool, input, `{"status_code":400,"error":"invalid recipient"}`, true)

	resp := confirmAudit(t, o, nil, legacy)
	if strings.Contains(resp.Content, "already confirmed") {
		t.Fatalf("re-audit after a failed call answered %q; it must re-grant the token for the retry", resp.Content)
	}
	if blocked, msg := o.checkCriticalTool(tool, "", input); blocked {
		t.Fatalf("retry after the re-audit refused: %q", msg)
	}
	o.recordToolResult(tool, input, `{"status_code":202}`, true)

	// After a successful call the shortcut is unchanged: an identical
	// re-audit is told the work is done.
	if resp := confirmAudit(t, o, nil, legacy); !strings.Contains(resp.Content, "Audit already confirmed") {
		t.Fatalf("identical re-audit after success answered %q, want the already-confirmed shortcut", resp.Content)
	}
}
