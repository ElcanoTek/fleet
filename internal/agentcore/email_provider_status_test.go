package agentcore

import "testing"

// A provider that rejects a send over a clean MCP transport answers with a
// non-2xx status_code and no "success"/"error" field (#1714). That is no
// delivery: the email's critical-action commitment must stay outstanding, the
// failure must count against the retry budget, and a later 2xx send must still
// discharge it. Covers both summary-email tools.
func TestEmailProviderRejectionDoesNotDischargeCommitment(t *testing.T) {
	for _, tool := range []string{"mcp_sendgrid_send_email", "mcp_sendgrid_send_template_email"} {
		for _, rejected := range []string{`{"status_code":500}`, `{"status_code":400,"body":"bad template"}`} {
			t.Run(tool+"/"+rejected, func(t *testing.T) {
				const args = `{"to_email":"trader@example.com","subject":"batch summary","content":"<p>done</p>"}`
				o := newOrchestrationState(nil, 0)
				if isBlocked, _ := o.checkCriticalTool(tool, "", args); !isBlocked {
					t.Fatal("a critical tool before confirm_audit must be blocked")
				}
				o.mu.Lock()
				o.auditConfirmed = true
				o.selfAuditRequested = true
				o.selfAuditConfirmedOnce = true
				o.mu.Unlock()

				o.recordToolResult(tool, args, rejected, true)
				if got := len(o.pendingCriticalActions); got != 1 {
					t.Fatalf("a provider-rejected send discharged the commitment: pending = %d, want 1", got)
				}
				if got := len(o.completedCriticalActions); got != 0 {
					t.Fatalf("a provider-rejected send was recorded as completed: %v", o.completedCriticalActions)
				}
				if got := o.criticalToolFailureAttempts[retryBudgetKey(tool, hashString(args))]; got != 1 {
					t.Fatalf("a provider-rejected send must count against the retry budget: attempts = %d, want 1", got)
				}

				o.recordToolResult(tool, args, `{"status_code":202,"status":"queued"}`, true)
				if got := len(o.pendingCriticalActions); got != 0 {
					t.Fatalf("a delivered send left %d pending action(s)", got)
				}
			})
		}
	}
}

func TestEmailProviderRejected(t *testing.T) {
	cases := []struct {
		tool, result string
		want         bool
	}{
		{"mcp_sendgrid_send_email", `{"status_code":500}`, true},
		{"mcp_ses_send_template_email", `{"status_code":403,"message":"denied"}`, true},
		{"send_email", `{"status_code":199}`, true},
		{"mcp_sendgrid_send_email", `{"status_code":202,"status":"queued"}`, false},
		{"mcp_ses_send_email", `{"status_code":200,"message_id":"x"}`, false},
		// No numeric status: left to mcpReportedFailure's convention.
		{"mcp_sendgrid_send_email", `{"success":true}`, false},
		{"mcp_sendgrid_send_email", `{"status_code":"500"}`, false},
		{"mcp_sendgrid_send_email", `queued`, false},
		{"mcp_sendgrid_send_email", ``, false},
		// Not an email tool: a status_code means whatever that tool says.
		{"mcp_http_fetch", `{"status_code":500}`, false},
	}
	for _, c := range cases {
		if got := emailProviderRejected(c.tool, c.result); got != c.want {
			t.Errorf("emailProviderRejected(%q, %q) = %v, want %v", c.tool, c.result, got, c.want)
		}
	}
}

func TestSendEmailSucceeded(t *testing.T) {
	cases := []struct {
		result string
		want   bool
	}{
		{`{"status_code":202,"status":"queued"}`, true},                         // SendGrid
		{`{"status_code":200,"message_id":"010f01a1","status":"queued"}`, true}, // Amazon SES
		{`{"status_code":204}`, true},
		{`{"status_code":199}`, false},
		{`{"status_code":300}`, false},
		{`{"status_code":500}`, false},
		{`{"status_code":200,"error":"boom"}`, false},
		{`{"status_code":"200"}`, false},
		{`{"status":"queued"}`, false},
		{`queued`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := sendEmailSucceeded(c.result); got != c.want {
			t.Errorf("sendEmailSucceeded(%q) = %v, want %v", c.result, got, c.want)
		}
	}
}
