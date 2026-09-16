package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

type completionBroker struct{ result string }

func (b completionBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return b.result, false, nil
}

// Exercise the real core -> scheduled log -> verifier seam, not a hand-built log.
func TestScheduledVerifierReceivesResultBeyondDisplayPreview(t *testing.T) {
	raw := `{"description":"` + strings.Repeat("contract text ", 600) + `","inspection":{"unchanged":true,"revision":91}}`
	session := NewLogSession()
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		calls++
		first := calls == 1
		return func(yield func(fantasy.StreamPart) bool) {
			if first {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "inspect", ToolCallName: "mcp_inventory_inspect", ToolCallInput: `{}`})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	_, err := agentcore.Run(context.Background(), agentcore.ModeInteractive, agentcore.RunConfig{}, agentcore.Deps{
		Input: scheduledInput{systemPrompt: "test", task: "inspect inventory"},
		Model: model, Policy: agentcore.NewInteractivePolicy(0, 0, nil, nil), LogSession: session,
		Observer: &scheduledObserver{session: session}, MCPBroker: completionBroker{raw},
		MCPCatalog: []mcp.ServerTool{{ServerName: "inventory", Tool: mcp.Tool{Name: "inspect", Description: "inspect inventory"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	records := buildToolExecSummary(session)
	if len(records) != 1 || !records[0].Succeeded || records[0].Result["inspection.unchanged"] != true {
		t.Fatalf("lost evidence after the display boundary: %+v", records)
	}
	for _, msg := range session.SnapshotMessages() {
		if msg.Role == roleTool && !json.Valid([]byte(msg.Content)) {
			t.Fatal("persisted truncated JSON")
		}
	}
}

type repairVerifierModel struct {
	itMockModel
	verdicts []string
	calls    int
}

func (m *repairVerifierModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	i := m.calls
	m.calls++
	if i >= len(m.verdicts) {
		i = len(m.verdicts) - 1
	}
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: m.verdicts[i]}}, FinishReason: fantasy.FinishReasonStop}, nil
}

func TestScheduledCompletionRechecksRepairsAndBoundsUnresolvedReviews(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdicts  []string
		wantError bool
		calls     int
	}{
		{"repaired", []string{`{"missing_actions":["verify inventory"]}`, `{"missing_actions":[]}`}, false, 2},
		{"unresolved", []string{`{"missing_actions":["verify inventory"]}`}, true, 3},
		{"malformed", []string{`not a verdict`}, true, 3},
		{"missing verdict", []string{`{}`}, true, 3},
		{"null verdict", []string{`{"missing_actions":null}`}, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &repairVerifierModel{verdicts: tc.verdicts}
			calls := 0
			model := &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				calls++
				first := calls == 1
				raw, _ := json.Marshal(call.Prompt)
				abort := strings.Contains(string(raw), "bounded repair attempts")
				return func(yield func(fantasy.StreamPart) bool) {
					if first || abort {
						input := `{"success":true,"critical_actions":[],"reasoning":"Inspected inventory","artifacts_checked":["inventory"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
						if abort {
							input = strings.Replace(input, `"success":true`, `"success":false,"user_visible_summary":"Completion remains unverified"`, 1)
						}
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				}, nil
			}}
			a := newTestScheduledAgent(t, model)
			a.fallbackModel = reviewer
			err := a.Execute(context.Background(), "Inspect inventory. No mutation is needed when unchanged.")
			if (err != nil) != tc.wantError || (tc.wantError && !errors.Is(err, agentcore.ErrAuditAborted)) {
				t.Fatalf("completion result: %v", err)
			}
			if reviewer.calls != tc.calls {
				t.Fatalf("verifier calls=%d, want %d", reviewer.calls, tc.calls)
			}
		})
	}
}
