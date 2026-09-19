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

func emailFrozenArgs() map[string]any {
	return map[string]any{
		"to_email":           "primary@example.com",
		"cc_emails":          []any{"copy@example.com"},
		"bcc_emails":         []any{"hidden@example.com"},
		"content":            strings.Repeat("long body ", 100) + "BODY_END",
		"attachments":        []any{map[string]any{"path": "report.csv"}},
		"inline_attachments": []any{map[string]any{"path": "chart.png", "cid": "logo"}},
	}
}

func frozenWire(args map[string]any, complete bool) map[string]any {
	return map[string]any{"complete": complete, "args": args}
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
	payload, _ := json.Marshal(frozenWire(emailFrozenArgs(), true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: conversation\ndata: {\"id\":\"c\"}\n\nevent: tool.approval_required\ndata: {\"approval_id\":\"a\",\"tool\":\"mcp_sendgrid_send_email\",\"frozen_args\":%s}\n\nevent: turn.completed\ndata: {}\n\n", payload)
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
			_ = json.NewEncoder(w).Encode(map[string]any{"pending_approvals": []any{map[string]any{"approval_id": "a", "tool": "mcp_sendgrid_send_email", "frozen_args": frozenWire(emailFrozenArgs(), true)}}})
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

func TestEmailReviewRefusesOverflowedBody(t *testing.T) {
	incomplete := frozenWire(emailFrozenArgs(), false)
	text := frozenApprovalReview(pendingApproval{tool: "mcp_sendgrid_send_email", frozenPresent: true, frozenComplete: false, frozenArgs: emailFrozenArgs()})
	if !strings.Contains(text, "INCOMPLETE") {
		t.Fatal(text)
	}
	var posted bool
	var out, errOut bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"pending_approvals": []any{map[string]any{"approval_id": "a", "tool": "mcp_sendgrid_send_email", "frozen_args": incomplete}}})
			return
		}
		posted = true
		fmt.Fprint(w, `{"status":"approved"}`)
	}))
	defer srv.Close()
	if code := runResolveApproval(NewClient(Config{ServerURL: srv.URL}), "c", "a", true, &out, &errOut); code != 1 {
		t.Fatal(code, errOut.String())
	}
	if posted {
		t.Fatal("truncated email body was approved")
	}
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "a", tool: "mcp_sendgrid_send_email", frozenPresent: true, frozenComplete: false, frozenArgs: emailFrozenArgs()}}
	if cmd := m.decideApproval([]string{"/approve"}, true); cmd != nil {
		t.Fatal("TUI posted an overflowed email")
	}
}

func TestEmailReviewEscapesTerminalControlBytes(t *testing.T) {
	text := frozenApprovalReview(withFrozen(pendingApproval{tool: "preview_email"}, map[string]any{"content": "\x1b[2J\rhidden"}))
	if strings.ContainsAny(text, "\x1b\r") || !strings.Contains(text, "hidden") {
		t.Fatal("unsafe terminal rendering", text)
	}
}

func TestTUIAutomaticallyPresentsFullFrozenBash(t *testing.T) {
	hidden := "git push origin main"
	cmd := strings.Repeat("echo safe; ", 20) + hidden
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "b",
		"tool":        "bash",
		"summary":     map[string]any{"command": cmd},
		"frozen_args": frozenWire(map[string]any{"command": cmd}, true),
	}})
	joined := strings.Join(m.approvalLines, "\n")
	if !strings.Contains(joined, hidden) {
		t.Fatal("TUI did not present the full frozen command before /approve")
	}
}

func TestFinishApprovalPinsSuggestedModel(t *testing.T) {
	m := newModel(Config{Model: "old/slug"})
	m.finishApproval(approvalResolvedMsg{approved: true, status: "approved", tool: "suggest_advanced_model", model: "acme/frontier-1-pro"})
	if m.client.cfg.Model != "acme/frontier-1-pro" {
		t.Fatalf("model = %q, want pinned suggestion", m.client.cfg.Model)
	}
}

func TestTUIAutomaticallyPresentsFullFrozenEmail(t *testing.T) {
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "a",
		"tool":        "mcp_sendgrid_send_email",
		"summary":     map[string]any{"to": "primary@example.com"},
		"frozen_args": frozenWire(emailFrozenArgs(), true),
	}})
	assertFullEmailReview(t, strings.Join(m.approvalLines, "\n"))
}
