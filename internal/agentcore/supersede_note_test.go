package agentcore

import (
	"strings"
	"testing"
)

// supersedingStager stages a card and reports that it replaced older ones.
type supersedingStager struct {
	sentinelStager
	replaced map[string]int64
}

func (s supersedingStager) SupersededCount(toolCallID string) int64 { return s.replaced[toolCallID] }

// A card that replaced older pending cards for the same tool says so, so the
// agent never tells the user those cards are still waiting (live 2026-11-23:
// "All 4 RTB rule updates have been staged" when three had been superseded).
func TestCriticalGateNamesSupersededCards(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"update_rtb_rule"}})

	o := newOrchestrationState(NewLogSession())
	o.setApprovalSink(supersedingStager{sentinelStager: sentinelStager{ret: "appr-9"}, replaced: map[string]int64{"call-2": 3}})
	blocked, msg := o.checkCriticalToolApproval("mcp_mg_update_rtb_rule", "call-2", `{"deal_id":1}`)
	if !blocked || !strings.Contains(msg, "approval_id=appr-9") {
		t.Fatalf("blocked=%v msg=%q, want the staged card", blocked, msg)
	}
	if !strings.Contains(msg, "REPLACED 3 older pending card(s)") || !strings.Contains(msg, "will NOT run") {
		t.Fatalf("msg = %q, want the superseded cards named", msg)
	}

	// Nothing replaced (or a stager that cannot tell): the message is unchanged.
	o2 := newOrchestrationState(NewLogSession())
	o2.setApprovalSink(supersedingStager{sentinelStager: sentinelStager{ret: "appr-10"}})
	if _, msg := o2.checkCriticalToolApproval("mcp_mg_update_rtb_rule", "call-3", `{}`); strings.Contains(msg, "REPLACED") {
		t.Fatalf("msg = %q, want no superseded note", msg)
	}
	o3 := newOrchestrationState(NewLogSession())
	o3.setApprovalSink(sentinelStager{ret: "appr-11"})
	if _, msg := o3.checkCriticalToolApproval("mcp_mg_update_rtb_rule", "call-4", `{}`); strings.Contains(msg, "REPLACED") {
		t.Fatalf("msg = %q, want no superseded note", msg)
	}
}

// batchStager records which staging entry point the gate used.
type batchStager struct {
	sentinelStager
	critical []string
	plain    []string
}

func (s *batchStager) Stage(toolName, _, _ string) (string, error) {
	s.plain = append(s.plain, toolName)
	return s.ret, nil
}

func (s *batchStager) StageCritical(toolName, _, _ string) (string, error) {
	s.critical = append(s.critical, toolName)
	return s.ret, nil
}

// The gate stages every critical call through StageCritical when the stager
// offers it, including a tool that is critical only because its MCP catalog
// entry says so (runCriticalTools, not a bundle suffix): the stager then keeps
// the turn's batch for it too (Codex P1 on #2231).
func TestCriticalGateStagesThroughStageCritical(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(testFixturePolicy()) })
	ConfigureAgentPolicy(AgentPolicy{CriticalToolSuffixes: []string{"update_rtb_rule"}})

	sink := &batchStager{sentinelStager: sentinelStager{ret: "appr-1"}}
	o := newOrchestrationState(NewLogSession())
	o.setApprovalSink(sink)
	o.runCriticalTools = map[string]string{"mcp_custom_post_thing": "post_thing"}

	for _, tool := range []string{"mcp_mg_update_rtb_rule", "mcp_custom_post_thing"} {
		if blocked, msg := o.checkCriticalToolApproval(tool, "call-"+tool, `{}`); !blocked || !strings.Contains(msg, "APPROVAL_REQUIRED") {
			t.Fatalf("%s: blocked=%v msg=%q, want a staged card", tool, blocked, msg)
		}
	}
	if len(sink.plain) != 0 || len(sink.critical) != 2 {
		t.Fatalf("StageCritical=%v Stage=%v; want both critical calls through StageCritical", sink.critical, sink.plain)
	}
}
