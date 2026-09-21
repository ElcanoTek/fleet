package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if len(records) != 1 || !records[0].Succeeded || records[0].Result["/inspection/unchanged"] != true {
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
		{"repaired at final review", []string{`{"missing_actions":["verify inventory"]}`, `{"missing_actions":["verify inventory"]}`, `{"missing_actions":[]}`}, false, 3},
		{"unresolved", []string{`{"missing_actions":["verify inventory"]}`}, true, 3},
		{"malformed", []string{`not a verdict`}, true, 3},
		{"missing verdict", []string{`{}`}, true, 3},
		{"null verdict", []string{`{"missing_actions":null}`}, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewer := &repairVerifierModel{verdicts: tc.verdicts}
			calls := 0
			model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				calls++
				first := calls == 1
				return func(yield func(fantasy.StreamPart) bool) {
					if first {
						input := `{"success":true,"critical_actions":[],"reasoning":"Inspected inventory","artifacts_checked":["inventory"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
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
			if (err != nil) != tc.wantError || (tc.wantError && !errors.Is(err, agentcore.ErrCompletionUnverified)) {
				t.Fatalf("completion result: %v", err)
			}
			// The dead-letter reason says what already went out (nothing here),
			// so an operator never re-runs a job whose send succeeded.
			if tc.wantError && !strings.Contains(err.Error(), "No connector call succeeded this run.") {
				t.Fatalf("exhausted verdict must name successful connector calls: %v", err)
			}
			if reviewer.calls != tc.calls {
				t.Fatalf("verifier calls=%d, want %d", reviewer.calls, tc.calls)
			}
			if calls > 4 {
				t.Fatalf("terminal verification failure kept driving the model: %d calls", calls)
			}
		})
	}
}

// A generic bundle-declared write exercises the same audited mutation path as
// a scheduled report refresh. The broker never touches an external service.
type reportCompletionBroker struct{ publishes, inspections int }

func (b *reportCompletionBroker) CallMCP(_ context.Context, _, tool string, _ map[string]any) (string, bool, error) {
	if tool == "publish_report" {
		b.publishes++
		return `{"published":true,"revision":92,"warnings":[],"profile":{"rows":{"count":83,"date_range":{"rows.date":["2026-09-01","2026-09-15"]},"totals":{"rows.revenue":1234.56789}}}}`, false, nil
	}
	b.inspections++
	return `{"ok":true,"revision":92,"schema_unchanged":true,"template_unchanged":true}`, false, nil
}

type reportCompletionReviewer struct {
	itMockModel
	t          *testing.T
	unresolved bool
	calls      int
}

func (m *reportCompletionReviewer) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	raw, _ := json.Marshal(call.Prompt)
	// Check the actual secondary-model input, across the complete core/observer
	// boundary, including both requested reconciliation and returned outcomes.
	// "Report published; inspection recorded." is the run's closing assistant
	// message: Gate 1 must hand it to the verifier as the FINAL RESPONSE
	// section (a "report X" task step is fulfilled there, not in a tool call).
	for _, field := range []string{
		"/expect/date_range/rows.date", "2026-09-01", "2026-09-15",
		"/expect/totals/rows.revenue", "1234.56789", "/expect/row_count/rows",
		"/profile/rows/totals/rows.revenue", "/published", "/ok", "/revision",
		"Never request replaying a successful mutation", "arguments_omitted", "result_omitted",
		"FINAL RESPONSE (the agent's closing message", "Report published; inspection recorded.",
	} {
		if !strings.Contains(string(raw), field) {
			m.t.Errorf("missing verifier evidence/instruction %q", field)
		}
	}
	verdict := `{"missing_actions":[]}`
	if m.unresolved {
		verdict = `{"missing_actions":["verify the existing report dimensions"]}`
	}
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: verdict}}, FinishReason: fantasy.FinishReasonStop}, nil
}

func TestScheduledCompletionAfterCommittedWrite(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"publish_report"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprintf("unresolved=%t", unresolved), func(t *testing.T) {
			broker := &reportCompletionBroker{}
			reviewer := &reportCompletionReviewer{t: t, unresolved: unresolved}
			calls := 0
			steps := []struct{ tool, input string }{
				{"confirm_audit", `{"success":true,"critical_actions":[{"tool":"mcp_reports_publish_report"}],"reasoning":"Reconciled report","artifacts_checked":["report.json"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`},
				{"mcp_reports_publish_report", `{"expected_revision":91,"expect":{"row_count":{"rows":83},"date_range":{"rows.date":["2026-09-01","2026-09-15"]},"totals":{"rows.revenue":1234.56789}}}`},
				{"mcp_reports_inspect", `{"revision":92}`},
			}
			model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				step := calls
				calls++
				return func(yield func(fantasy.StreamPart) bool) {
					if step < len(steps) {
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: fmt.Sprint(step), ToolCallName: steps[step].tool, ToolCallInput: steps[step].input})
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Report published; inspection recorded."})
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5}})
				}, nil
			}}
			a := newTestScheduledAgent(t, model)
			a.fallbackModel = reviewer
			a.mcpBroker = broker
			a.mcpCatalog = []mcp.ServerTool{
				{ServerName: "reports", Tool: mcp.Tool{Name: "publish_report", Description: "Publish the reconciled report"}},
				{ServerName: "reports", Tool: mcp.Tool{Name: "inspect", Description: "Inspect the live report"}},
			}
			err := a.Execute(context.Background(), "Publish the report with expected row count, date range and totals; verify the resulting revision.")
			if unresolved {
				if !errors.Is(err, agentcore.ErrCompletionUnverified) || errors.Is(err, agentcore.ErrMaxEnforcementRounds) {
					t.Fatalf("want bounded verification failure, got %v", err)
				}
				if reviewer.calls != 3 || calls != 6 {
					t.Fatalf("review/abort loop: reviews=%d model calls=%d", reviewer.calls, calls)
				}
				logJSON, _ := json.Marshal(a.logSession.SnapshotMessages())
				for _, text := range []string{"completion_unverified", "1 critical actions completed", "have not been rolled back", "Report published; inspection recorded."} {
					if !strings.Contains(string(logJSON), text) {
						t.Errorf("lost terminal evidence: %s", text)
					}
				}
				for _, text := range []string{"published nothing", "Audit Abort Refused", "round_cap_truncated"} {
					if strings.Contains(string(logJSON), text) {
						t.Errorf("misleading or looping terminal result: %s", text)
					}
				}
			} else if err != nil || reviewer.calls != 1 {
				t.Fatalf("successful publication was rejected: %v (reviews=%d)", err, reviewer.calls)
			}
			if broker.publishes != 1 || broker.inspections != 1 {
				t.Fatalf("unexpected external actions: %+v", broker)
			}
			if a.logSession.PromptTokens == 0 {
				t.Fatal("terminal verification dropped usage")
			}
		})
	}
}
