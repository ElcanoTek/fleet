package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A preview is display-only — there is no action for default-deny to protect
// against — so it stages with NO deadline. Everything else keeps its window.
func TestExpiryUnixFor_PreviewNeverExpires(t *testing.T) {
	a := &approvalStager{globalTimeoutSeconds: 120}
	if got := a.expiryUnixFor("preview_email"); got != 0 {
		t.Errorf("preview_email expiry = %d, want 0 (no deadline)", got)
	}
	if got := a.expiryUnixFor("mcp_sendgrid_send_email"); got <= time.Now().Unix() {
		t.Errorf("send_email expiry = %d, want a future deadline", got)
	}
}

// Legacy pending preview rows (staged with a deadline before previews stopped
// expiring) must sweep with honest copy: nothing was denied because nothing
// was ever going to run.
func TestTimeoutResultTextFor(t *testing.T) {
	if got := timeoutResultTextFor("preview_email"); got != previewExpiredResultText {
		t.Errorf("preview text = %q, want the display-only wording", got)
	}
	if got := timeoutResultTextFor("mcp_pages_deploy_page"); got != approvalTimeoutResultText {
		t.Errorf("action text = %q, want the auto-deny wording", got)
	}
	if !strings.HasPrefix(approvalTimeoutResultText, "Approval timed out") {
		t.Error("the UI matches on the 'Approval timed out' prefix for its ask-again affordance; keep it stable")
	}
}

// The decline wording follows the tool kind. A declined pages deploy used to
// be recorded as "User declined to send this email" — wrong in the transcript
// and wrong for the next turn's model.
func TestRejectionMessagesPerToolKind(t *testing.T) {
	if _, history := rejectionMessages("mcp_sendgrid_send_email"); !strings.Contains(history, "email") {
		t.Errorf("email decline = %q, want email wording", history)
	}
	if _, history := rejectionMessages(tools.ManageTasksToolName); strings.Contains(history, "email") {
		t.Errorf("manage_tasks decline = %q, must not claim an email was involved", history)
	}
	claim, history := rejectionMessages("mcp_pages_deploy_page")
	if strings.Contains(history, "email") || strings.Contains(claim, "send") {
		t.Errorf("generic decline = (%q, %q), must not use email/send wording", claim, history)
	}
	if !strings.Contains(history, "mcp_pages_deploy_page") {
		t.Errorf("generic decline = %q, want it to name the declined tool", history)
	}
}

// The generic card shows a tool's arguments verbatim — sorted keys (JSON maps
// have no order), nested values compacted, everything rune-truncated. Fleet
// cannot know what a bundle tool's arguments mean, so verbatim is the honest
// floor for a human review step.
func TestSummarizeGenericToolInput(t *testing.T) {
	sum := summarizeGenericToolInput("mcp_pages_deploy_page",
		`{"slug":"q3-report","config":{"campaign":"fall"},"publish":true}`)
	if sum["tool"] != "mcp_pages_deploy_page" {
		t.Fatalf("tool = %v", sum["tool"])
	}
	rows, ok := sum["args"].([]map[string]string)
	if !ok || len(rows) != 3 {
		t.Fatalf("args = %#v, want 3 key/value rows", sum["args"])
	}
	if rows[0]["key"] != "config" || rows[1]["key"] != "publish" || rows[2]["key"] != "slug" {
		t.Errorf("keys = %v %v %v, want sorted config/publish/slug", rows[0]["key"], rows[1]["key"], rows[2]["key"])
	}
	if rows[0]["value"] != `{"campaign":"fall"}` {
		t.Errorf("nested value = %q, want compact JSON", rows[0]["value"])
	}
	if rows[1]["value"] != "true" || rows[2]["value"] != "q3-report" {
		t.Errorf("scalar values = %q, %q", rows[1]["value"], rows[2]["value"])
	}

	// Long values truncate by runes so a multibyte payload can't split.
	long := summarizeGenericToolInput("t", `{"html":"`+strings.Repeat("é", genericArgValueMax+10)+`"}`)
	longRows := long["args"].([]map[string]string)
	if got := []rune(longRows[0]["value"]); len(got) != genericArgValueMax+1 { // +1 for the ellipsis
		t.Errorf("truncated value runes = %d, want %d", len(got), genericArgValueMax+1)
	}

	// Unparseable args degrade to the raw payload, matching the other cards.
	broken := summarizeGenericToolInput("t", "{not json")
	if broken["raw"] != "{not json" {
		t.Errorf("broken summary = %v, want the raw payload", broken)
	}
}

// The email-shaped summary is reserved for tools that actually send email;
// everything else routes to the generic tool summary. This is the server half
// of retiring the "pages deploy renders as 'Send this email?'" fallthrough.
func TestSummarizeApprovalInput_RoutesNonEmailToolsGeneric(t *testing.T) {
	generic := summarizeApprovalInput("mcp_pages_deploy_page", `{"slug":"x"}`, "")
	if _, hasSubject := generic["subject"]; hasSubject {
		t.Error("generic tool summary must not carry email fields")
	}
	if _, hasArgs := generic["args"]; !hasArgs {
		t.Error("generic tool summary must carry args rows")
	}
	email := summarizeApprovalInput("mcp_sendgrid_send_email", `{"to_email":"a@b.c","subject":"s","content":"hi"}`, "")
	if email["subject"] != "s" {
		t.Errorf("email summary lost its shape: %v", email)
	}
}

// Notify-mode records are recognized by their stable result prefix — the one
// durable trace of HOW the row resolved (the table stores no mode column).
func TestIsNotifyRecordResult(t *testing.T) {
	if !isNotifyRecordResult(notifyRecordResultPrefix + ": this tool is declared notify-mode in the client bundle. Undo with rollback.") {
		t.Error("record text must be recognized")
	}
	if isNotifyRecordResult("User declined this action.") {
		t.Error("a decline is not a record")
	}
}

// expiredClickFakeStore backs the expired-click test: a pending row whose
// deadline already passed, plus the claim + history the resolution writes.
type expiredClickFakeStore struct {
	*store.Store
	approval     store.Approval
	claimedText  string
	claimedID    string
	appendedText string
}

func (f *expiredClickFakeStore) GetApproval(_ context.Context, _, _ string) (*store.Approval, error) {
	a := f.approval
	return &a, nil
}

func (f *expiredClickFakeStore) ClaimExpiredApproval(_ context.Context, _, approvalID, _, resultText string) (bool, error) {
	f.claimedID = approvalID
	f.claimedText = resultText
	return true, nil
}

func (f *expiredClickFakeStore) AppendHistory(_ context.Context, _ string, entries []agent.HistoryEntry) ([]int64, error) {
	if len(entries) == 1 {
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(entries[0].Content, &c)
		f.appendedText = c.Text
	}
	return nil, nil
}

// A click that lands after the deadline but before the sweep's next tick used
// to lose the claim and get the row's still-pending state echoed back — the
// card silently reset and the user could click forever. It now resolves the
// row as timed out at click time, deterministically.
func TestHandleApproval_ExpiredClickResolvesTimeoutNow(t *testing.T) {
	fake := &expiredClickFakeStore{approval: store.Approval{
		ID:             "ap1",
		ConversationID: "c1",
		UserEmail:      "u@x.com",
		ToolName:       "mcp_pages_deploy_page",
		ToolCallID:     "tc1",
		Status:         "pending",
		ExpiresAt:      time.Now().Unix() - 30,
	}}
	s := &Server{store: fake}

	req := httptest.NewRequest("POST", "/conversations/c1/approvals/ap1",
		strings.NewReader(`{"approved":true}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c1", "ap1")

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status     string `json:"status"`
		ResultText string `json:"result_text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if resp.Status != "rejected" {
		t.Errorf("status = %q, want rejected (default-deny is authoritative at click time)", resp.Status)
	}
	if resp.ResultText != approvalTimeoutResultText {
		t.Errorf("result_text = %q, want the timeout wording", resp.ResultText)
	}
	if fake.claimedID != "ap1" || fake.claimedText != approvalTimeoutResultText {
		t.Errorf("claim = (%q, %q), want the sweep-equivalent expired claim", fake.claimedID, fake.claimedText)
	}
	if fake.appendedText != approvalTimeoutResultText {
		t.Errorf("history breadcrumb = %q, want the timeout wording so the next turn's model knows", fake.appendedText)
	}
}

// Every card preview truncates by RUNES so a multibyte payload is never cut
// mid-character into a replacement glyph; the 1 MiB email content cap is a
// byte budget snapped back to a rune boundary.
func TestCardPreviewsTruncateOnRuneBoundaries(t *testing.T) {
	if got := excerpt(strings.Repeat("é", 50), 10); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 11 {
		t.Errorf("excerpt = %q (runes=%d), want 10 runes + ellipsis, valid UTF-8", got, utf8.RuneCountInString(got))
	}
	if got := excerpt("short", 10); got != "short" {
		t.Errorf("excerpt under the cap = %q, want unchanged", got)
	}

	long := strings.Repeat("é", 700)
	bash := summarizeBashInput("bash", `{"command":"`+long+`"}`)
	if p := bash["preview"].(string); !utf8.ValidString(p) || !strings.HasPrefix(p, strings.Repeat("é", 600)+"…") {
		t.Errorf("bash preview cut mid-rune or at the wrong length: runes=%d", utf8.RuneCountInString(p))
	}

	email := summarizeSendEmailInput("send_email", `{"content":"`+long+`","content_type":"text/plain"}`, "")
	if p := email["preview"].(string); !utf8.ValidString(p) || !strings.HasPrefix(p, strings.Repeat("é", 600)+"…") {
		t.Errorf("email preview cut mid-rune or at the wrong length: runes=%d", utf8.RuneCountInString(p))
	}

	// Content over 1 MiB: cut at a rune boundary, flagged as overflowed.
	huge := strings.Repeat("é", (1<<20)/2+5) // 2-byte runes, over the byte cap by an odd margin
	email = summarizeSendEmailInput("send_email", `{"content":"`+huge+`","content_type":"text/plain"}`, "")
	content := email["content"].(string)
	if !utf8.ValidString(content) || len(content) > 1<<20 {
		t.Errorf("email content cap: valid=%v len=%d", utf8.ValidString(content), len(content))
	}
	if overflow, _ := email["content_overflow"].(bool); !overflow {
		t.Errorf("content over the cap should be flagged: %v", email["content_overflow"])
	}
}
