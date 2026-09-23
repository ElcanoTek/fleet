package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestToolResultLooksFailed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		failed  bool
	}{
		{"fantasy error result", "[tool error] connection refused", true},
		{"top-level error string", `{"error":"upstream 400"}`, true},
		{"top-level error object", `{"error":{"message":"HTTP 400"}}`, true},
		{"null error is not a failure", `{"error":null,"id":"v42"}`, false},
		{"explicit success wins over an error field", `{"success":true,"error":""}`, false},
		{"fantasy error result no message", "[tool error] (no message)", true},
		{"legacy compact status error", `{"status":"error","message":"boom"}`, true},
		{"status error with spaces", `{"status": "error", "message": "boom"}`, true},
		{"status error reordered keys", `{"message":"boom","status":"error"}`, true},
		{"loop guard block", "LOOP_GUARD (block #1): this exact call ...", true},
		{"audit block", "BLOCKED: 'send_email' requires audit first.", true},
		{"safety limit block", "Safety Limit: send_email already executed 3 times.", true},
		{"safety guard block", "Safety Guard: Duplicate send_email blocked.", true},
		// The duplicate-send suppression means the send already succeeded — the
		// verifier must count the action satisfied or it re-demands a call the
		// guard will never allow (the demand/refuse deadlock of #1153's era).
		{"duplicate send suppressed", "Duplicate send_email suppressed: an identical payload was already sent successfully by this run, so this send is complete.", false},
		{"duplicate send suppressed behind error prefix", "[tool error] Duplicate send_email suppressed: an identical payload was already sent successfully by this run.", false},
		{"unsuccessful JSON", `{"success":false}`, true},
		{"failed preflight", `{"ok":false}`, true},
		{"incomplete staging is a valid call", `{"complete":false}`, false},
		{"plain success text", "Email queued successfully", false},
		{"status success json", `{"status":"success","message_id":"abc"}`, false},
		{"json without status", `{"rows": 12, "summary": "ok"}`, false},
		{"empty result", "", false},
		{"error mentioned mid-text", `Report complete. Note: 0 errors encountered.`, false},
		{"truncated status error json", `{"status":"error","message":"boom`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolResultLooksFailed(tc.content); got != tc.failed {
				t.Fatalf("toolResultLooksFailed(%q) = %v, want %v", tc.content, got, tc.failed)
			}
		})
	}
}

// TestBuildToolExecSummary_FantasyErrorResultsCountAsFailed pins the fix for
// the verifier blind spot: blocked critical tools, MCP isError=true results,
// and tool exceptions are all logged with a "[tool error] " prefix by
// extractToolResultText, and the end-of-run verifier must see them as
// failures — not as successful executions of send_email / deal creation.
func TestBuildToolExecSummary_FantasyErrorResultsCountAsFailed(t *testing.T) {
	session := NewLogSession()
	callID1, callID2, callID3 := "c1", "c2", "c3"
	session.Messages = []LogMessage{
		{
			Role: roleAssistant,
			ToolCalls: []LogToolCall{
				{ID: callID1, Name: "mcp_sendgrid_send_email"},
				{ID: callID2, Name: "run_python"},
				{ID: callID3, Name: "view_file"},
			},
		},
		{Role: roleTool, ToolCallID: &callID1, Content: "[tool error] BLOCKED: 'mcp_sendgrid_send_email' requires audit first."},
		{Role: roleTool, ToolCallID: &callID2, Content: `{"status":"error","message":"traceback"}`},
		{Role: roleTool, ToolCallID: &callID3, Content: "file contents here"},
	}

	records := buildToolExecSummary(session)
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}
	byName := map[string]bool{}
	for _, r := range records {
		byName[r.Name] = r.Succeeded
	}
	if byName["mcp_sendgrid_send_email"] {
		t.Fatal("blocked send_email must be reported as failed to the verifier")
	}
	if byName["run_python"] {
		t.Fatal("status=error tool result must be reported as failed")
	}
	if !byName["view_file"] {
		t.Fatal("ordinary text result must be reported as succeeded")
	}
}

// promptCapturingVerifierModel records the verifier prompt so a test can assert
// exactly what the fallback model was shown, across the real Agent seam.
type promptCapturingVerifierModel struct {
	itMockModel
	prompt string
}

func (m *promptCapturingVerifierModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	raw, _ := json.Marshal(call.Prompt)
	m.prompt = string(raw)
	return &fantasy.Response{Content: []fantasy.Content{fantasy.TextContent{Text: `{"missing_actions":[],"reasoning":"The final response reports the outcome and the publish succeeded."}`}}, FinishReason: fantasy.FinishReasonStop}, nil
}

// TestRunEndOfRunVerifierIncludesFinalResponse pins the fix for the production
// dead-letters: a task step phrased "Report X" is fulfilled in the run's
// closing assistant message, which the verifier never saw — so it re-demanded
// the report on every check and a run that did the work and said so still
// dead-lettered as unverified. The verifier prompt must carry the final
// response as its own clearly delimited section, exactly as Gate 1 passes it.
func TestRunEndOfRunVerifierIncludesFinalResponse(t *testing.T) {
	session := NewLogSession()
	callID := "c1"
	session.Messages = []LogMessage{
		{Role: roleAssistant, ToolCalls: []LogToolCall{{ID: callID, Name: "mcp_pages_publish", Arguments: `{"version":856}`}}},
		{Role: roleTool, ToolCallID: &callID, Content: `{"published":true,"version":856}`},
		{Role: roleAssistant, Content: "Report: refreshed to version 856, verified live and published. Coverage window 2026-09-01 through 2026-09-15."},
	}
	finalText := latestAssistantText(session)
	if finalText == "" {
		t.Fatal("setup: expected a final assistant message")
	}

	model := &promptCapturingVerifierModel{}
	a := &Agent{fallbackModel: model, logSession: session}
	missing, err := a.runEndOfRunVerifier(context.Background(), "Refresh the page and report the outcome", finalText, buildToolExecSummary(session))
	if err != nil || len(missing) != 0 {
		t.Fatalf("verifier result: %v, %v", missing, err)
	}
	for _, want := range []string{
		"ORIGINAL TASK",
		"TOOL EXECUTIONS",
		"FINAL RESPONSE (the agent's closing message",
		finalText,
	} {
		if !strings.Contains(model.prompt, want) {
			t.Errorf("verifier prompt missing %q", want)
		}
	}
}

// TestRunEndOfRunVerifierEmptyFinalResponseMarked: when the run left no
// assistant text, the verifier must see an EXPLICIT marker — a blank section
// would read as "nothing to check" and a task that demanded a report could
// never be flagged for the absence.
func TestRunEndOfRunVerifierEmptyFinalResponseMarked(t *testing.T) {
	model := &promptCapturingVerifierModel{}
	a := &Agent{fallbackModel: model, logSession: NewLogSession()}
	missing, err := a.runEndOfRunVerifier(context.Background(), "Report the outcome", "", nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("verifier result: %v, %v", missing, err)
	}
	if !strings.Contains(model.prompt, verifierNoFinalResponseMarker) {
		t.Errorf("verifier prompt missing the explicit empty marker %q", verifierNoFinalResponseMarker)
	}
}

// TestTruncateFinalResponseForVerifier pins the bound: empty/whitespace becomes
// the explicit marker, a short response passes through unchanged, and an
// oversized one keeps the head AND the tail with the cut marked — the opening
// summary and the closing details are both evidence.
func TestTruncateFinalResponseForVerifier(t *testing.T) {
	if got := truncateFinalResponseForVerifier(""); got != verifierNoFinalResponseMarker {
		t.Errorf("empty = %q, want %q", got, verifierNoFinalResponseMarker)
	}
	if got := truncateFinalResponseForVerifier("   \n\t "); got != verifierNoFinalResponseMarker {
		t.Errorf("whitespace-only = %q, want %q", got, verifierNoFinalResponseMarker)
	}
	short := "Published version 856; coverage window 2026-09-01..2026-09-15."
	if got := truncateFinalResponseForVerifier(short); got != short {
		t.Errorf("short response = %q, want unchanged %q", got, short)
	}
	if got := truncateFinalResponseForVerifier("  " + short + "\n"); got != short {
		t.Errorf("padded response = %q, want trimmed %q", got, short)
	}

	long := strings.Repeat("a", verifierMaxFinalResponseChars+500)
	got := truncateFinalResponseForVerifier(long)
	marker := "\n…[middle truncated for verifier]\n"
	if len(got) != verifierMaxFinalResponseChars+len(marker) {
		t.Errorf("truncated length = %d, want %d (%d head+tail + marked cut)", len(got), verifierMaxFinalResponseChars+len(marker), verifierMaxFinalResponseChars)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", verifierMaxFinalResponseChars/2)) {
		t.Error("truncation lost the head")
	}
	if !strings.HasSuffix(got, strings.Repeat("a", verifierMaxFinalResponseChars/2)) {
		t.Error("truncation lost the tail")
	}
	if !strings.Contains(got, "middle truncated for verifier") {
		t.Error("truncation must mark the cut")
	}
}
