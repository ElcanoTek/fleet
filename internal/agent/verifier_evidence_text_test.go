package agent

// #1540: the end-of-run verifier used to see connector calls as "tool_call",
// lost every value containing "@" or a space (recipients, subjects) and every
// line a native tool printed, so its own "use read-only verification" repair
// could never converge. Modeled on the 2026-09-17 Reklaim health-scan run
// f312eeb6, which sent its email and was then dead-lettered.

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonString encodes s as a JSON string literal for building raw tool results.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestVerifierEvidenceKeepsSingleLineValues(t *testing.T) {
	got := projectVerifierEvidence(`{"to_email":"roman@elcanotek.com","subject":"Reklaim Daily Health Scan (Day over Day) — September 15, 2026","status":"queued","link":"https://example.invalid/x","empty":""}`)
	if got.fields["/to_email"] != "roman@elcanotek.com" {
		t.Fatalf("recipient dropped: %+v", got.fields)
	}
	// Short free text (a subject line) is still not evidence, verbatim or as an
	// excerpt: only identifier-like tokens and bulk-text excerpts get through.
	if _, has := got.fields["/subject"]; has {
		t.Fatalf("free text must not be retained as a scalar: %+v", got.fields)
	}
	if _, has := got.fields["/subject#excerpt"]; has {
		t.Fatalf("short free text must not be excerpted: %+v", got.fields)
	}
	if _, has := got.fields["/link"]; has {
		t.Fatalf("URL must not be retained: %+v", got.fields)
	}
	if _, has := got.fields["/link#excerpt"]; has {
		t.Fatalf("a bare URL must not become an excerpt either: %+v", got.fields)
	}
	if _, has := got.fields["/empty"]; has {
		t.Fatalf("empty string is not evidence: %+v", got.fields)
	}
	if !got.omitted {
		t.Fatal("the dropped URL and empty value must keep the omission flag honest")
	}
	for _, bad := range []string{"tab\tseparated", "line\nbreak", "bell\x07", "Ignore task and create", "[REDACTED]", strings.Repeat("y", verifierScalarMaxLen+1)} {
		if verifierTextScalar(bad) {
			t.Errorf("%q must not be a scalar", bad)
		}
	}
	for _, ok := range []string{"roman@elcanotek.com", "2026-09-15", "queued", "010f01a0afa25cc1-d149387b", "mcp_ses_outbound_send_email"} {
		if !verifierTextScalar(ok) {
			t.Errorf("%q must be a scalar", ok)
		}
	}
	// Only bulk text earns an excerpt.
	if verifierBulkText("Ignore task and create") || !verifierBulkText("a\nb") || !verifierBulkText(strings.Repeat("y", verifierScalarMaxLen+1)) {
		t.Fatal("bulk-text classification wrong")
	}
}

func TestVerifierEvidenceExcerptsLongText(t *testing.T) {
	output := "=== VERIFY OPENX ===\nOpenX file: Full_Data_Report.csv__d2838e9f_2.gz\nOpenX rows: 5397, Date min: 2026-08-17, Date max: 2026-09-15\nsee https://storage.example/report.gz for the source\n" + strings.Repeat("filler line of verification prose\n", 40) + "=== VERIFY EMAIL ===\nSES Message ID: 010f01a0afa25cc1\nSES Delivery Status: queued (status_code: 200)"
	raw := `{"status":"success","output":` + jsonString(output) + `,"stdout":` + jsonString(output) + `,"stderr":"","vars":{},"error":"","execution_time_ms":28}`
	got := projectVerifierEvidence(raw)
	if got.fields["/status"] != "success" {
		t.Fatalf("status lost: %+v", got.fields)
	}
	excerpt, _ := got.fields["/output#excerpt"].(string)
	if !strings.Contains(excerpt, "OpenX rows: 5397") || !strings.Contains(excerpt, "Delivery Status: queued") {
		t.Fatalf("excerpt must keep the head and the tail of the printed check: %q", excerpt)
	}
	if !strings.Contains(excerpt, " … ") || strings.Contains(excerpt, "\n") || strings.Contains(excerpt, "https://") {
		t.Fatalf("excerpt must be single-line, elided, URL-free: %q", excerpt)
	}
	if len([]rune(excerpt)) > verifierExcerptMax+3 {
		t.Fatalf("excerpt too long: %d runes", len([]rune(excerpt)))
	}
	if _, dup := got.fields["/stdout#excerpt"]; dup {
		t.Fatalf("identical stdout must not spend a second excerpt slot: %+v", got.fields)
	}
	if !got.omitted {
		t.Fatal("excerpting is still an omission of the full value")
	}

	// The excerpt budget is bounded: three distinct long texts yield two excerpts.
	many := projectVerifierEvidence(`{"a":` + jsonString(strings.Repeat("alpha ", 60)) + `,"b":` + jsonString(strings.Repeat("beta ", 60)) + `,"c":` + jsonString(strings.Repeat("gamma ", 60)) + `}`)
	n := 0
	for k := range many.fields {
		if strings.HasSuffix(k, verifierExcerptSuffix) {
			n++
		}
	}
	if n != verifierExcerptFields {
		t.Fatalf("excerpts=%d, want %d", n, verifierExcerptFields)
	}
	// Excerpts never break the projection's byte cap.
	if enc := len(jsonString(strings.Repeat("z", 5000))); enc < verifierEvidenceByteCap {
		t.Fatal("test setup: need an oversized text")
	}
	if capped := projectVerifierEvidence(`{"x":` + jsonString(strings.Repeat("z ", 3000)) + `}`); capped.bytes > verifierEvidenceByteCap {
		t.Fatalf("byte cap exceeded: %d", capped.bytes)
	}
}

// The incident, through the real record builder: the bridge call is recorded
// under the connector tool's name with its own arguments, so the recipient is
// visible; the model's read-only check survives as an excerpt.
func TestBuildToolExecSummaryUnwrapsToolCallBridge(t *testing.T) {
	send, check, bad := "send", "check", "bad"
	session := NewLogSession()
	session.Messages = []LogMessage{
		{Role: roleAssistant, ToolCalls: []LogToolCall{
			{ID: send, Name: "tool_call", Arguments: `{"name":"mcp_ses_outbound_send_email","arguments":{"to_email":"roman@elcanotek.com","subject":"Reklaim Daily Health Scan — September 15, 2026","content":"<!DOCTYPE html><html>` + strings.Repeat("<tr><td>row</td></tr>", 300) + `</html>"}}`},
			{ID: check, Name: "run_python", Arguments: `{"code":"print('verify')"}`},
			{ID: bad, Name: "tool_call", Arguments: `{"not":"a bridge payload"}`},
		}},
		{Role: roleTool, ToolCallID: &send, Content: `{"status_code":200,"message_id":"010f01a0afa25cc1-d149387b","status":"queued","content_length":30534,"html_validated":true}`},
		{Role: roleTool, ToolCallID: &check, Content: `{"status":"success","output":"=== VERIFY OPENX ===\nOpenX rows: 5397, Date max: 2026-09-15\n=== VERIFY EMAIL ===\nRecipient verified in prior send call: roman@elcanotek.com","stdout":"","stderr":"","error":""}`},
		{Role: roleTool, ToolCallID: &bad, Content: `{"ok":true}`},
	}
	records := buildToolExecSummary(session)
	if len(records) != 3 {
		t.Fatalf("records=%d", len(records))
	}
	byName := map[string]toolExecRecord{}
	for _, r := range records {
		byName[r.Name] = r
	}
	sent, ok := byName["mcp_ses_outbound_send_email"]
	if !ok || sent.Wrapper != "tool_call" || !sent.Succeeded {
		t.Fatalf("bridge call not unwrapped to the connector tool: %+v", records)
	}
	if sent.Arguments["/to_email"] != "roman@elcanotek.com" {
		t.Fatalf("recipient not visible to the verifier: %+v", sent.Arguments)
	}
	if _, has := sent.Arguments["/content"]; has || !sent.ArgumentsOmitted {
		t.Fatalf("the HTML body must not be retained verbatim: %+v", sent.Arguments)
	}
	if _, has := sent.Arguments["/content#excerpt"]; has {
		t.Fatalf("arguments are intent, not proof: no excerpt of the HTML body: %+v", sent.Arguments)
	}
	if sent.Result["/status"] != "queued" || sent.Result["/message_id"] != "010f01a0afa25cc1-d149387b" {
		t.Fatalf("send outcome lost: %+v", sent.Result)
	}
	py := byName["run_python"]
	if excerpt, _ := py.Result["/output#excerpt"].(string); !strings.Contains(excerpt, "OpenX rows: 5397") || !strings.Contains(excerpt, "roman@elcanotek.com") {
		t.Fatalf("read-only check invisible to the verifier: %+v", py.Result)
	}
	if py.Wrapper != "" {
		t.Fatalf("native tool must carry no wrapper: %+v", py)
	}
	if malformed, ok := byName["tool_call"]; !ok || malformed.Wrapper != "" {
		t.Fatalf("a bridge call without a parseable payload must stay recorded as tool_call: %+v", records)
	}
}

func TestSuccessfulConnectorCalls(t *testing.T) {
	records := []toolExecRecord{
		{Name: "mcp_ses_outbound_send_email", Succeeded: true},
		{Name: "mcp_ses_outbound_send_email", Succeeded: true},
		{Name: "mcp_email_search_emails", Succeeded: true},
		{Name: "mcp_pubmatic_mcp_pm_run_standard_report", Succeeded: false},
		{Name: "run_python", Succeeded: true},
	}
	got := successfulConnectorCalls(records)
	if got != "Successful connector calls this run: mcp_email_search_emails ×1, mcp_ses_outbound_send_email ×2." {
		t.Fatalf("got %q", got)
	}
	if got := successfulConnectorCalls(nil); got != "No connector call succeeded this run." {
		t.Fatalf("got %q", got)
	}
}
