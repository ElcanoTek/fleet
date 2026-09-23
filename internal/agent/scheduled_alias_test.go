package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// pagesWriteBroker answers the two Pages write variants and counts each.
type pagesWriteBroker struct{ calls map[string]int }

func (b *pagesWriteBroker) CallMCP(_ context.Context, _, tool string, _ map[string]any) (string, bool, error) {
	b.calls[tool]++
	return `{"ok":true,"version":{"id":"42"},"published":true}`, false, nil
}

// cleanReviewer is the end-of-run verifier: nothing is missing.
type cleanReviewer struct {
	itMockModel
	calls int
}

func (m *cleanReviewer) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: `{"missing_actions":[]}`}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// The prod failure of #1604, end to end through the scheduled driver: the audit
// declares the inline Pages write, the payload goes out through the staged
// upload twin. With the bundle's critical_tool_aliases the upload discharges
// the declaration, so the run finishes on its first finish attempt — no
// "declared but not executed" enforcement, no abort, one write.
func TestScheduledAliasedWriteDischargesTheDeclaredCommitment(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{"update_page_data", "update_page_data_upload"},
		CriticalToolAliases:  map[string][]string{"update_page_data": {"update_page_data_upload"}},
	})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })

	steps := []struct{ tool, input string }{
		{"confirm_audit", `{"success":true,"critical_actions":[{"tool":"mcp_pages_update_page_data"}],"reasoning":"Reconciled the refreshed data","artifacts_checked":["page_data.json"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`},
		{"mcp_pages_update_page_data_upload", `{"slug":"page-a","upload_id":"u-1","expected_version":41}`},
	}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			if step < len(steps) {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: fmt.Sprint(step), ToolCallName: steps[step].tool, ToolCallInput: steps[step].input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Page A refreshed; live version 42."})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}})
		}, nil
	}}
	broker := &pagesWriteBroker{calls: map[string]int{}}
	reviewer := &cleanReviewer{}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = reviewer
	a.mcpBroker = broker
	a.mcpCatalog = []mcp.ServerTool{
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data", Description: "Replace a page's data inline"}},
		{ServerName: "pages", Tool: mcp.Tool{Name: "update_page_data_upload", Description: "Replace a page's data from a staged upload"}},
	}

	if err := a.Execute(context.Background(), "Refresh the page A data."); err != nil {
		t.Fatalf("a run whose declared write landed through its alias must succeed, got %v", err)
	}
	if broker.calls["update_page_data_upload"] != 1 || broker.calls["update_page_data"] != 0 {
		t.Fatalf("writes = %v, want exactly the one upload", broker.calls)
	}
	if calls != len(steps)+1 || reviewer.calls != 1 {
		t.Fatalf("model calls = %d, reviews = %d: want the first finish accepted (%d calls, 1 review)", calls, reviewer.calls, len(steps)+1)
	}
	logJSON, _ := json.Marshal(a.logSession.SnapshotMessages())
	for _, text := range []string{"have not successfully executed", "matches no outstanding audited commitment", "Audit Failed", "BLOCKED"} {
		if strings.Contains(string(logJSON), text) {
			t.Errorf("the run was told %q — the aliased write should have discharged the declaration", text)
		}
	}
}
