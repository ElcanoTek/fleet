package agent

import (
	"testing"
)

func TestToolResultLooksFailed(t *testing.T) {
	cases := []struct {
		name    string
		content string
		failed  bool
	}{
		{"fantasy error result", "[tool error] connection refused", true},
		{"fantasy error result no message", "[tool error] (no message)", true},
		{"legacy compact status error", `{"status":"error","message":"boom"}`, true},
		{"status error with spaces", `{"status": "error", "message": "boom"}`, true},
		{"status error reordered keys", `{"message":"boom","status":"error"}`, true},
		{"loop guard block", "LOOP_GUARD (block #1): this exact call ...", true},
		{"audit block", "BLOCKED: 'send_email' requires audit first.", true},
		{"safety limit block", "Safety Limit: send_email already executed 3 times.", true},
		{"safety guard block", "Safety Guard: Duplicate send_email blocked.", true},
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
