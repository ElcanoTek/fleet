package chattui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func emailReviewFixture() map[string]any {
	return map[string]any{
		"to": "primary@example.com", "cc": []string{"copy@example.com"}, "bcc": []string{"hidden@example.com"},
		"content":            strings.Repeat("long body ", 100) + "BODY_END",
		"attachments":        []any{map[string]any{"path": "report.csv"}},
		"inline_attachments": []any{map[string]any{"path": "chart.png", "cid": "logo"}},
	}
}

func assertFullEmailReview(t *testing.T, text string) {
	t.Helper()
	for _, want := range []string{"primary@example.com", "copy@example.com", "hidden@example.com", "BODY_END", "report.csv", "chart.png", "logo"} {
		if !strings.Contains(text, want) {
			t.Errorf("email review missing %s", want)
		}
	}
}

func TestOneShotPrintsFullFrozenEmailBeforeSettlementCommand(t *testing.T) {
	summary, _ := json.Marshal(emailReviewFixture())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: conversation\ndata: {\"id\":\"c\"}\n\nevent: tool.approval_required\ndata: {\"approval_id\":\"a\",\"tool\":\"mcp_sendgrid_send_email\",\"summary\":%s}\n\nevent: turn.completed\ndata: {}\n\n", summary)
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := runOneShot(NewClient(Config{ServerURL: srv.URL}), "", "preview", strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	assertFullEmailReview(t, errOut.String())
	if strings.Index(errOut.String(), "BODY_END") > strings.Index(errOut.String(), "settle it:") {
		t.Fatal("review printed after settlement instructions")
	}
}

func TestCLIApprovalReviewsEmailBeforePost(t *testing.T) {
	var out, errOut bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"pending_approvals": []any{map[string]any{"approval_id": "a", "tool": "mcp_sendgrid_send_email", "summary": emailReviewFixture()}}})
			return
		}
		assertFullEmailReview(t, errOut.String())
		fmt.Fprint(w, `{"status":"approved"}`)
	}))
	defer srv.Close()
	if code := runResolveApproval(NewClient(Config{ServerURL: srv.URL}), "c", "a", true, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
}

func TestEmailReviewEscapesTerminalControlBytes(t *testing.T) {
	text := emailApprovalReview("preview_email", map[string]any{"content": "\x1b[2J\rhidden"})
	if strings.ContainsAny(text, "\x1b\r") || !strings.Contains(text, "hidden") {
		t.Fatal("unsafe terminal rendering", text)
	}
}

func TestTUIAutomaticallyPresentsFullFrozenEmail(t *testing.T) {
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{"approval_id": "a", "tool": "mcp_sendgrid_send_email", "summary": emailReviewFixture()}})
	assertFullEmailReview(t, strings.Join(m.approvalLines, "\n"))
}
