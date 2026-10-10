package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_resume matches by suffix like critical_tools, and a bundle
// without it opts nothing in.
func TestResumeAfterApproval(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if ResumeAfterApprovalEnabled() || ResumeAfterApproval("mcp_deals_update_deal") || ResumeAfterApproval("send_email") {
		t.Fatal("with no declaration no tool may resume")
	}
	if got := ResumeMaxPerHour(); got != DefaultResumeMaxPerHour {
		t.Fatalf("default cap = %d, want %d", got, DefaultResumeMaxPerHour)
	}

	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:         []string{"update_deal", "deploy_page"},
		CriticalToolResume:           []string{" update_deal ", ""},
		CriticalToolResumeMaxPerHour: 4,
	})
	if !ResumeAfterApprovalEnabled() {
		t.Fatal("a declared member must enable the feature")
	}
	for _, tool := range []string{"update_deal", "mcp_deals_update_deal", "mcp_deals_client_a_update_deal"} {
		if !ResumeAfterApproval(tool) {
			t.Errorf("%s: no resume, want resume", tool)
		}
	}
	for _, tool := range []string{"mcp_pages_deploy_page", "mcp_deals_update_deals", "mcp_deals_update_deal_preview", "mcp_sendgrid_send_email"} {
		if ResumeAfterApproval(tool) {
			t.Errorf("%s: resumes, want no resume (not declared)", tool)
		}
	}
	if got := ResumeMaxPerHour(); got != 4 {
		t.Fatalf("cap = %d, want the declared 4", got)
	}
}

// A member that is not critical (no card is ever staged), or that runs in
// notify mode (no card either), is reported by validate-config and dropped at
// boot; so is an out-of-range cap, which falls back to the default.
func TestResumeAfterApprovalProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes:         []string{"update_deal", "deploy_page"},
		CriticalToolModes:            map[string]string{"deploy_page": "notify"},
		CriticalToolResume:           []string{"update_deal", "send_email", "updat_deal", "deploy_page"},
		CriticalToolResumeMaxPerHour: 500,
	}
	problems := strings.Join(ResumeAfterApprovalProblems(p), "\n")
	for _, want := range []string{`"updat_deal"`, `"deploy_page"`, "critical_tool_resume_max_per_hour 500"} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems %q do not name %s", problems, want)
		}
	}
	if strings.Contains(problems, `"send_email"`) || strings.Contains(problems, `member "update_deal"`) {
		t.Errorf("a critical, card-staging member must not be reported: %q", problems)
	}
	ConfigureAgentPolicy(p)
	if ResumeAfterApproval("mcp_x_updat_deal") || ResumeAfterApproval("mcp_pages_deploy_page") {
		t.Error("a reported member must not be installed")
	}
	if !ResumeAfterApproval("mcp_x_update_deal") || !ResumeAfterApproval("mcp_sendgrid_send_email") {
		t.Error("valid members must still be installed")
	}
	if got := ResumeMaxPerHour(); got != DefaultResumeMaxPerHour {
		t.Errorf("out-of-range cap installed as %d, want the default %d", got, DefaultResumeMaxPerHour)
	}
	if got := ResumeAfterApprovalProblems(AgentPolicy{}); len(got) != 0 {
		t.Errorf("no declaration must report nothing, got %v", got)
	}
}
