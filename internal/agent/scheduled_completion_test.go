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
	withFastVerifierRetry(t)
	for _, tc := range []struct {
		name      string
		verdicts  []string
		wantError bool
		calls     int
		// wantWarning: whether the run finished with the
		// completion_unverified_verifier_error warning (#1602). A verifier that
		// ANSWERS with something that is not a verdict is a content failure,
		// not an outage: each check is retried once and then spent, as before
		// — so these three still dead-letter, after six calls.
		wantWarning bool
	}{
		{"repaired", []string{`{"missing_actions":["verify inventory"]}`, `{"missing_actions":[]}`}, false, 2, false},
		{"repaired at final review", []string{`{"missing_actions":["verify inventory"]}`, `{"missing_actions":["verify inventory"]}`, `{"missing_actions":[]}`}, false, 3, false},
		{"unresolved", []string{`{"missing_actions":["verify inventory"]}`}, true, 3, false},
		{"malformed", []string{`not a verdict`}, true, 2 * maxCompletionVerifications, false},
		{"missing verdict", []string{`{}`}, true, 2 * maxCompletionVerifications, false},
		{"null verdict", []string{`{"missing_actions":null}`}, true, 2 * maxCompletionVerifications, false},
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
			if got := hasSessionMessageType(a.logSession, agentcore.MessageTypeCompletionUnverifiedVerifierError); got != tc.wantWarning {
				t.Fatalf("completion_unverified_verifier_error warning recorded=%t, want %t", got, tc.wantWarning)
			}
		})
	}
}

// gateOneCapturingVerifier approves every verification but captures each
// prompt it was handed, so a test can pin exactly what Gate 1 saw.
type gateOneCapturingVerifier struct {
	itMockModel
	prompts []string
}

func (m *gateOneCapturingVerifier) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	raw, _ := json.Marshal(call.Prompt)
	m.prompts = append(m.prompts, string(raw))
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: `{"missing_actions":[]}`}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// reviewerForcesRepair returns a needs_revision verdict with one actionable
// issue. Gate 2 is single-shot, so this is called at most once per run.
type reviewerForcesRepair struct {
	itMockModel
	t *testing.T
}

func (m *reviewerForcesRepair) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	raw, _ := json.Marshal(call.Prompt)
	if !strings.Contains(string(raw), "First answer.") {
		m.t.Errorf("reviewer should critique the first answer, prompt missing it")
	}
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: `{"needs_revision":true,"issues":["the summary omits the coverage window"],"reasoning":"the first answer is incomplete"}`}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// TestScheduledReviewerRepairReverifies pins the Gate 2 → Gate 1 invalidation:
// the reviewer approves nothing here — it forces a repair, and the repaired
// answer must go through the verifier again. Pre-fix the verifier ran once
// (verified stayed true after the reviewer's repair round), so a repair could
// swap in an unverified answer and still finish. The repair round's text is
// the corrected answer: it REPLACES the reviewed one (never appended after
// the wrong answer), and it is what the run persists.
func TestScheduledReviewerRepairReverifies(t *testing.T) {
	verifier := &gateOneCapturingVerifier{}
	reviewer := &reviewerForcesRepair{t: t}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0:
				input := `{"success":true,"critical_actions":[],"reasoning":"Reconciled report","artifacts_checked":["report"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "First answer."})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Revised answer with the coverage window 2026-09-01..2026-09-15."})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier
	a.reviewerModel = reviewer
	a.phoneAFriendEnabled = true

	err := a.Execute(context.Background(), "Summarise the report and state the coverage window.")
	if err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(verifier.prompts) != 2 {
		t.Fatalf("verifier calls = %d, want 2 — the reviewer-forced repair must be re-verified", len(verifier.prompts))
	}
	if !strings.Contains(verifier.prompts[0], "First answer.") {
		t.Error("first verification must judge the first answer")
	}
	if !strings.Contains(verifier.prompts[1], "Revised answer with the coverage window") {
		t.Error("second verification must judge the repaired round's text")
	}
	if strings.Contains(verifier.prompts[1], "First answer.") {
		t.Error("second verification carried the reviewed answer — a reviewer repair replaces it")
	}
	want := "Revised answer with the coverage window 2026-09-01..2026-09-15."
	if got := lastAssistantContent(a.logSession); got != want {
		t.Errorf("persisted answer = %q, want the corrected answer alone %q", got, want)
	}
}

// TestComposeRunAnswer pins how a repair round's text combines with the
// answer a gate judged: a true supplement is appended, a restatement that
// contains the judged text (or is contained in it, or equals it with
// different whitespace) keeps only the longer text, and a reviewer repair
// replaces the judged text unless the repair round produced none.
func TestComposeRunAnswer(t *testing.T) {
	const report = "Refresh published. Coverage 2026-09-01..2026-09-15."
	for _, tc := range []struct {
		name          string
		judged, round string
		replace       bool
		want          string
	}{
		{"supplement appended", report, "Report dates skipped: none.", false, report + "\n\nReport dates skipped: none."},
		{"restated report plus the missing item kept once", report, report + "\nReport dates skipped: none.", false, report + "\nReport dates skipped: none."},
		{"repeat with different whitespace kept once", report, "  Refresh published.\n\nCoverage   2026-09-01..2026-09-15. ", false, "Refresh published.\n\nCoverage   2026-09-01..2026-09-15."},
		{"a fragment of the judged text adds nothing", report, "Coverage 2026-09-01..2026-09-15.", false, report},
		{"textless round keeps the judged answer", report, "", false, report},
		{"nothing judged yet", "", "First answer.", false, "First answer."},
		{"reviewer repair replaces", "Wrong answer.", "Corrected answer.", true, "Corrected answer."},
		{"textless reviewer repair keeps the reviewed answer", "Reviewed answer.", "  ", true, "Reviewed answer."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := composeRunAnswer(tc.judged, tc.round, tc.replace); got != tc.want {
				t.Fatalf("composeRunAnswer = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScheduledVerifierRepairThatRestatesTheReportPersistsItOnce: a model that
// answers the repair by restating its whole report plus the missing item must
// not persist the report twice.
func TestScheduledVerifierRepairThatRestatesTheReportPersistsItOnce(t *testing.T) {
	verifier := &supplementRepairVerifierModel{}
	const first = "Refresh published. Coverage 2026-09-01..2026-09-15."
	const restated = first + " Report dates skipped: none."
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0:
				input := `{"success":true,"critical_actions":[],"reasoning":"Refreshed the page","artifacts_checked":["page"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: first})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: restated})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier
	if err := a.Execute(context.Background(), "Refresh the page, then report the coverage window and any report dates skipped."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if got := lastAssistantContent(a.logSession); got != restated {
		t.Fatalf("persisted answer = %q, want the restated report once %q", got, restated)
	}
}

// lastAssistantContent is the run's persisted final answer: the content of the
// session's last assistant message, as the runner's successMessage reads it.
func lastAssistantContent(session *LogSession) string {
	messages := session.SnapshotMessages()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == roleAssistant {
			return messages[i].Content
		}
	}
	return ""
}

// issuesOnceReviewer flags needs_revision with one actionable issue, exactly
// once — Gate 2 is single-shot per run, so it is never called again.
type issuesOnceReviewer struct {
	itMockModel
	calls int
}

func (m *issuesOnceReviewer) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: `{"needs_revision":true,"issues":["the summary omits the coverage window"],"reasoning":"incomplete"}`}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// TestScheduledReviewerRepairExhaustsVerificationCap pins the cap arithmetic
// across a reviewer-forced repair: reject, reject, accept (the 3-call cap is
// spent), the reviewer forces a repair, and the re-check must NOT call the
// verifier a fourth time — the run ends as ErrCompletionUnverified through the
// exhaustion path, because exhaustion never grants success.
func TestScheduledReviewerRepairExhaustsVerificationCap(t *testing.T) {
	verifier := &repairVerifierModel{verdicts: []string{
		`{"missing_actions":["run the read-only verification"]}`,
		`{"missing_actions":["run the read-only verification"]}`,
		`{"missing_actions":[]}`,
	}}
	reviewer := &issuesOnceReviewer{}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			if step == 0 {
				input := `{"success":true,"critical_actions":[],"reasoning":"Reconciled report","artifacts_checked":["report"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Answer, revised."})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier
	a.reviewerModel = reviewer
	a.phoneAFriendEnabled = true

	err := a.Execute(context.Background(), "Summarise the report and state the coverage window.")
	if !errors.Is(err, agentcore.ErrCompletionUnverified) {
		t.Fatalf("run error = %v, want ErrCompletionUnverified", err)
	}
	if !strings.Contains(err.Error(), "could not be re-verified within the cap") {
		t.Errorf("exhausted repair reason = %v, want the reviewer-repair detail", err)
	}
	if verifier.calls != 3 {
		t.Fatalf("verifier calls = %d, want exactly 3 — a reviewer-forced repair must not buy a fourth verification", verifier.calls)
	}
	if reviewer.calls != 1 {
		t.Fatalf("reviewer calls = %d, want 1 (single-shot)", reviewer.calls)
	}
}

// textlessRepairVerifierModel rejects the first verification (demanding a
// read-only check) and approves the second, capturing every prompt it was
// handed so the test can pin exactly what the verifier saw at each gate.
type textlessRepairVerifierModel struct {
	itMockModel
	t       *testing.T
	prompts []string
}

func (m *textlessRepairVerifierModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	raw, _ := json.Marshal(call.Prompt)
	m.prompts = append(m.prompts, string(raw))
	verdict := `{"missing_actions":[]}`
	if len(m.prompts) == 1 {
		verdict = `{"missing_actions":["run the read-only verification read"]}`
	}
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: verdict}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// TestScheduledVerifierTextlessRepairRoundKeepsJudgedAnswer is the Codex P1
// scenario from the verifier's FINAL RESPONSE change, under the rule that the
// gates judge what the run persists: round 1 closes with a prose report and
// the verifier rejects it for a missing action; the repair round makes ONLY
// the tool call and leaves no assistant text. The re-check judges the report
// the verifier already saw plus the fresh tool evidence — and that report is
// the answer the run persists, so a report that never happened cannot be
// approved (the original P1: the gate saw an earlier draft while completeRun
// persisted the textless round's empty text as success).
func TestScheduledVerifierTextlessRepairRoundKeepsJudgedAnswer(t *testing.T) {
	verifier := &textlessRepairVerifierModel{t: t}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0:
				input := `{"success":true,"critical_actions":[],"reasoning":"Reconciled report","artifacts_checked":["report"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Report: all done, v856 live."})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			default:
				// The repair tail: the model makes the tool call (step 2) and any
				// follow-up rounds produce NO assistant text at all.
				if step == 2 {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "verify", ToolCallName: "mcp_reports_verify", ToolCallInput: `{"revision":92}`})
				}
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			}
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier
	a.mcpBroker = completionBroker{`{"ok":true,"revision":92}`}
	a.mcpCatalog = []mcp.ServerTool{
		{ServerName: "reports", Tool: mcp.Tool{Name: "verify", Description: "Verify the live report"}},
	}

	err := a.Execute(context.Background(), "Publish the report and verify the resulting revision.")
	if err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(verifier.prompts) != 2 {
		t.Fatalf("verifier calls = %d, want 2 (reject, then re-check)", len(verifier.prompts))
	}
	// Gate 1 after the prose round sees the report.
	if !strings.Contains(verifier.prompts[0], "Report: all done, v856 live.") {
		t.Error("first gate must carry the round's closing report")
	}
	// The re-check after the textless repair round judges the run's answer —
	// the judged report — never the no-response marker for text that exists.
	if !strings.Contains(verifier.prompts[1], "Report: all done, v856 live.") {
		t.Error("second gate lost the judged report the run persists")
	}
	if strings.Contains(verifier.prompts[1], verifierNoFinalResponseMarker) {
		t.Errorf("second gate showed the %q marker for a run that has an answer", verifierNoFinalResponseMarker)
	}
	if !strings.Contains(verifier.prompts[1], "/ok") || !strings.Contains(verifier.prompts[1], "/revision") {
		t.Error("second gate must still carry the repair round's fresh tool evidence")
	}
	if got := lastAssistantContent(a.logSession); got != "Report: all done, v856 live." {
		t.Errorf("persisted answer = %q, want the judged report", got)
	}
}

// supplementRepairVerifierModel stands in for the production verifier: it
// approves only a FINAL RESPONSE that names both requested items, and records
// every prompt.
type supplementRepairVerifierModel struct {
	itMockModel
	prompts []string
}

func (m *supplementRepairVerifierModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	raw, _ := json.Marshal(call.Prompt)
	m.prompts = append(m.prompts, string(raw))
	_, response, _ := strings.Cut(string(raw), "FINAL RESPONSE (")
	var missing []string
	for _, item := range []string{"Coverage 2026-09-01..2026-09-15", "Report dates skipped: none"} {
		if !strings.Contains(response, item) {
			missing = append(missing, "report "+item)
		}
	}
	verdict, _ := json.Marshal(map[string]any{"missing_actions": append([]string{}, missing...), "reasoning": "checked the final response"})
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: string(verdict)}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// TestScheduledVerifierRepairSupplementPassesOnCombinedAnswer is the production
// reproduction (Pages refresh, 2026-09): the verifier rejects the first report
// for one missing item, the repair round answers with ONLY the missing
// sentence, and the run used to dead-letter after three checks because each
// re-check judged that sentence alone. The re-check must judge the combined
// answer, pass, and persist that same combined text as the run's result.
func TestScheduledVerifierRepairSupplementPassesOnCombinedAnswer(t *testing.T) {
	verifier := &supplementRepairVerifierModel{}
	calls := 0
	var repairNudge string
	model := &itMockModel{streamFunc: func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		if step == 2 {
			raw, _ := json.Marshal(call.Prompt)
			repairNudge = string(raw)
		}
		return func(yield func(fantasy.StreamPart) bool) {
			switch step {
			case 0:
				input := `{"success":true,"critical_actions":[],"reasoning":"Refreshed the page","artifacts_checked":["page"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			case 1:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Refresh published. Coverage 2026-09-01..2026-09-15."})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "Report dates skipped: none."})
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			}
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier

	err := a.Execute(context.Background(), "Refresh the page, then report the coverage window and any report dates skipped.")
	if err != nil {
		t.Fatalf("run rejected although the combined answer has every item: %v", err)
	}
	if len(verifier.prompts) != 2 {
		t.Fatalf("verifier calls = %d, want 2 (reject the first report, approve the supplemented one)", len(verifier.prompts))
	}
	if !strings.Contains(repairNudge, repairAnswerNote) {
		t.Error("the repair nudge must tell the model its next text is appended to the answer")
	}
	want := "Refresh published. Coverage 2026-09-01..2026-09-15.\n\nReport dates skipped: none."
	if !strings.Contains(verifier.prompts[1], "Refresh published. Coverage 2026-09-01..2026-09-15.\\n\\nReport dates skipped: none.") {
		t.Errorf("re-check did not judge the combined answer: %s", verifier.prompts[1])
	}
	if got := lastAssistantContent(a.logSession); got != want {
		t.Errorf("persisted answer = %q, want the verified combined answer %q", got, want)
	}
}

// TestScheduledVerifierTextlessRunSeesNoResponseMarker keeps the explicit
// marker for a run that produced no assistant text in any round: the gate must
// be able to flag a demanded report that is genuinely absent.
func TestScheduledVerifierTextlessRunSeesNoResponseMarker(t *testing.T) {
	verifier := &textlessRepairVerifierModel{t: t}
	calls := 0
	model := &itMockModel{streamFunc: func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		step := calls
		calls++
		return func(yield func(fantasy.StreamPart) bool) {
			if step == 0 {
				input := `{"success":true,"critical_actions":[],"reasoning":"Reconciled report","artifacts_checked":["report"],"workflow_sections_checked":["completion"],"send_contract_checked":true,"attachments_checked":[],"remaining_risks":[]}`
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "audit", ToolCallName: "confirm_audit", ToolCallInput: input})
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	}}
	a := newTestScheduledAgent(t, model)
	a.fallbackModel = verifier

	if err := a.Execute(context.Background(), "Reconcile the report and state the outcome."); err != nil {
		t.Fatalf("run rejected: %v", err)
	}
	if len(verifier.prompts) != 2 {
		t.Fatalf("verifier calls = %d, want 2", len(verifier.prompts))
	}
	for i, prompt := range verifier.prompts {
		if !strings.Contains(prompt, verifierNoFinalResponseMarker) {
			t.Errorf("gate %d missing the %q marker for a textless run", i+1, verifierNoFinalResponseMarker)
		}
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
