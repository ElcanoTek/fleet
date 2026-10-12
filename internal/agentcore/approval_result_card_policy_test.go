package agentcore

import (
	"strings"
	"testing"
)

// critical_tool_result_cards matches by suffix like critical_tools, and a
// bundle without it opts nothing in.
func TestApprovalResultCard(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })

	ConfigureAgentPolicy(AgentPolicy{})
	if ApprovalResultCard("mcp_pubmatic_execute_plan") {
		t.Fatal("with no declaration no result may carry a card")
	}
	ConfigureAgentPolicy(AgentPolicy{
		CriticalToolSuffixes:    []string{"execute_plan"},
		CriticalToolResultCards: []string{"execute_plan"},
	})
	for _, tool := range []string{"execute_plan", "mcp_pubmatic_execute_plan", "mcp_magnite_client_a_execute_plan"} {
		if !ApprovalResultCard(tool) {
			t.Errorf("%s: no result card, want one", tool)
		}
	}
	if ApprovalResultCard("mcp_pubmatic_execute_plans") || ApprovalResultCard("mcp_sendgrid_send_email") {
		t.Error("undeclared tools must not read a result card")
	}
}

// A member that is not critical, or names a handler-only card, is reported and
// dropped at boot. A notify-mode member is kept: its record is settled by the
// call's result too.
func TestResultCardProblems(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{
		CriticalToolSuffixes:    []string{"execute_plan", "next_campaign_id", "schedule_task"},
		CriticalToolModes:       map[string]string{"next_campaign_id": "notify"},
		CriticalToolResultCards: []string{"execute_plan", "execute_plans", "next_campaign_id", "schedule_task", " ", "execute_plan"},
	}
	problems := ResultCardProblems(p)
	joined := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(joined, `"execute_plans"`) || !strings.Contains(joined, `"schedule_task"`) {
		t.Fatalf("problems = %q, want the two bad members", joined)
	}
	ConfigureAgentPolicy(p)
	if !ApprovalResultCard("mcp_x_execute_plan") || !ApprovalResultCard("mcp_deal_sheet_next_campaign_id") || ApprovalResultCard("schedule_task") {
		t.Fatal("valid members installed (notify kept), reported ones dropped")
	}
}
