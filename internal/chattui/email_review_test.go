package chattui

import (
	"bytes"
	"context"
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

func TestSSEEscapedEmailFitsTransport(t *testing.T) {
	body := strings.Repeat("<", 950<<10)
	encoded, err := json.Marshal(map[string]any{
		"approval_id": "large", "tool": "preview_email",
		"pattern_args": map[string]any{"content": body},
		"summary":      map[string]any{"content": body},
		"frozen_args":  frozenWire(map[string]any{"content": body}, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= 16<<20 {
		t.Fatal("fixture must exceed the old scanner cap")
	}
	var got Event
	err = parseSSE(strings.NewReader("event: tool.approval_required\ndata: "+string(encoded)+"\n\n"), func(ev Event) { got = ev })
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := got.Data["frozen_args"].(json.RawMessage)
	if !ok {
		t.Fatal("missing frozen arguments")
	}
	args, complete, present := parseFrozenArgs(raw)
	if !present || !complete || args["content"] != body {
		t.Fatal("transport truncated frozen email")
	}
}

func TestInteractiveReloadFetchesLargeCardsIndividually(t *testing.T) {
	body := strings.Repeat("<", 950<<10)
	var fetched int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("approval_index") == "1" {
			fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"a"},{"approval_id":"b"},{"approval_id":"c"}]}`)
			return
		}
		id := r.URL.Query().Get("approval_id")
		if id == "" {
			http.Error(w, "aggregate request refused", http.StatusRequestEntityTooLarge)
			return
		}
		fetched++
		_ = json.NewEncoder(w).Encode(map[string]any{"pending_approvals": []any{map[string]any{
			"approval_id": id, "tool": "mcp_" + id + "_send_email", "summary": map[string]any{"content": body},
			"frozen_args": frozenWire(map[string]any{"content": body}, true),
		}}})
	}))
	defer srv.Close()
	cards, err := NewClient(Config{ServerURL: srv.URL}).loadApprovals(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if fetched != 3 || len(cards) != 3 {
		t.Fatalf("fetched=%d cards=%d", fetched, len(cards))
	}
	for _, card := range cards {
		if card.frozenArgs["content"] != body || !card.reviewComplete() {
			t.Fatal("large card truncated")
		}
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
			if r.URL.Query().Get("approval_id") != "a" {
				t.Error("one-shot review downloaded unrelated pending approvals")
				http.Error(w, "approval filter required", http.StatusRequestEntityTooLarge)
				return
			}
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

func TestCLIApprovalRetriesSettledOutcomeAfterLostResponse(t *testing.T) {
	var committed bool
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if committed {
				fmt.Fprint(w, `{"resolved_approvals":[{"approval_id":"a","tool":"bash","status":"approved","is_err":false}]}`)
			} else {
				fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"a","tool":"bash","frozen_args":{"complete":true,"args":{"command":"echo hi"}}}]}`)
			}
			return
		}
		posts++
		if !committed {
			committed = true
			http.Error(w, "response lost after commit", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"status":"approved","result_text":"recorded result","is_err":false}`)
	}))
	defer srv.Close()
	client := NewClient(Config{ServerURL: srv.URL})
	var out, errOut bytes.Buffer
	if code := runResolveApproval(client, "c", "a", true, &out, &errOut); code != 1 {
		t.Fatalf("first response should fail: %d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := runResolveApproval(client, "c", "a", true, &out, &errOut); code != 0 {
		t.Fatalf("recorded result was not retrievable: %d %s", code, errOut.String())
	}
	if posts != 2 || !strings.Contains(out.String(), "recorded result") || strings.Contains(errOut.String(), "INCOMPLETE") {
		t.Fatalf("replay posts=%d stdout=%q stderr=%q", posts, out.String(), errOut.String())
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
	m.convID = "suggested"
	m.finishApproval(approvalResolvedMsg{approved: true, status: "approved", tool: "suggest_advanced_model", model: "acme/frontier-1-pro"})
	if m.client.displayModel(m.convID) != "acme/frontier-1-pro" {
		t.Fatalf("model = %q, want pinned suggestion", m.client.displayModel(m.convID))
	}
	if got := m.client.turnModel(m.convID); got != "" {
		t.Fatalf("cached pin would overwrite another client's selection: %q", got)
	}
	m.runSlash("/model explicit/new")
	if got := m.client.turnModel(m.convID); got != "explicit/new" {
		t.Fatalf("explicit operator override lost: %q", got)
	}
	m.client.cfg.Model = "old/slug"
	m.runSlash("/new")
	if got := m.client.turnModel(m.convID); got != "old/slug" {
		t.Fatalf("new conversation lost explicit CLI override: %q", got)
	}
	m.client.cfg.Model = ""
	m.client.AdoptDefaultModel("workspace/default")
	if got := m.client.turnModel(""); got != "workspace/default" {
		t.Fatalf("new conversation inherited suggestion: %q", got)
	}
	if got := m.client.turnModel("other"); got != "" {
		t.Fatalf("resume would override stored model: %q", got)
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
