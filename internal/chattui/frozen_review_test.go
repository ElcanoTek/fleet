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

func withFrozen(a pendingApproval, args map[string]any) pendingApproval {
	if args == nil {
		args = map[string]any{}
	}
	a.frozenPresent = true
	a.frozenComplete = true
	a.frozenArgs = args
	return a
}

func TestGenericAndScheduleFrozenReviewShowsHiddenSuffix(t *testing.T) {
	hidden := "HIDDEN_DEPLOY_TARGET"
	html := strings.Repeat("x", 300) + hidden
	card := withFrozen(pendingApproval{id: "g", tool: "mcp_pages_deploy_page"}, map[string]any{
		"html": html,
		"url":  "https://evil.example",
	})
	line := approvalSummaryLine(card.tool, map[string]any{"tool": card.tool, "args": []any{map[string]any{"key": "html", "value": html[:160]}}})
	if strings.Contains(line, hidden) {
		t.Fatal("one-line summary should stay truncated")
	}
	review := frozenApprovalReview(card)
	if !strings.Contains(review, hidden) || !strings.Contains(review, "https://evil.example") {
		t.Fatal(review)
	}
	if strings.ContainsAny(review, "\x1b\r") {
		t.Fatal("review leaked controls")
	}

	taskTail := "MALICIOUS_TASK_TAIL"
	prompt := strings.Repeat("benign ", 40) + taskTail
	sched := withFrozen(pendingApproval{id: "s", tool: "schedule_task"}, map[string]any{
		"name":   "nightly",
		"prompt": prompt,
		"cron":   "0 9 * * *",
	})
	if !strings.Contains(frozenApprovalReview(sched), taskTail) {
		t.Fatal("schedule review hid the prompt tail")
	}
}

func TestFrozenReviewEscapesControlsAndBidi(t *testing.T) {
	card := withFrozen(pendingApproval{tool: "bash"}, map[string]any{
		"command": "echo \x1b[2J\r\u009b\u202e hidden",
	})
	review := frozenApprovalReview(card)
	if strings.ContainsAny(review, "\x1b\r") || strings.ContainsRune(review, '\u009b') || strings.ContainsRune(review, '\u202e') {
		t.Fatal("unsafe terminal rendering", review)
	}
	if !strings.Contains(review, "hidden") {
		t.Fatal(review)
	}
}

func TestIncompleteFrozenPayloadsRefuseApprove(t *testing.T) {
	cases := []pendingApproval{
		{id: "missing", tool: "mcp_pages_deploy_page"},
		{id: "nil-map", tool: "bash", frozenPresent: false},
		{id: "incomplete", tool: "schedule_task", frozenPresent: true, frozenComplete: false, frozenArgs: map[string]any{"prompt": "x"}},
		{id: "typed-nil-args", tool: "bash", frozenPresent: true, frozenComplete: true, frozenArgs: nil},
	}
	for _, card := range cases {
		if card.reviewComplete() {
			t.Fatalf("%s should not be complete", card.id)
		}
		m := newModel(Config{})
		m.pending = []pendingApproval{card}
		if cmd := m.decideApproval([]string{"/approve"}, true); cmd != nil {
			t.Fatalf("%s posted an incomplete snapshot", card.id)
		}
		if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "Refusing to approve") {
			t.Fatalf("%s: %s", card.id, joined)
		}
		if len(m.pending) != 1 {
			t.Fatalf("%s dequeued the card", card.id)
		}
	}
}

func TestCLIAndAutoResolveFailClosedOnIncompleteFrozenArgs(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pending_approvals": []any{
					map[string]any{"approval_id": "legacy", "tool": "mcp_pages_deploy_page", "summary": map[string]any{"html": "prefix"}},
					map[string]any{"approval_id": "truncated", "tool": "schedule_task", "frozen_args": map[string]any{"complete": false}},
					map[string]any{"approval_id": "null-args", "tool": "bash", "frozen_args": map[string]any{"complete": true, "args": nil}},
				},
			})
			return
		}
		posted = true
		fmt.Fprint(w, `{"status":"approved"}`)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL})
	for _, id := range []string{"legacy", "truncated", "null-args", "missing"} {
		var out, errOut bytes.Buffer
		if code := runResolveApproval(c, "c", id, true, &out, &errOut); code != 1 {
			t.Fatalf("%s: code %d %s", id, code, errOut.String())
		}
	}
	if posted {
		t.Fatal("incomplete snapshots were approved")
	}

	m := newModel(Config{})
	m.convID = "c"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {{Approved: true, Scope: "session"}}},
	}
	m.pending = []pendingApproval{{id: "legacy", tool: "schedule_task"}}
	if m.autoResolveCard() != nil {
		t.Fatal("auto-approve must not settle an incomplete snapshot")
	}
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "ok", tool: "schedule_task"}, nil)}
	if m.autoResolveCard() == nil {
		t.Fatal("complete snapshot should still auto-resolve")
	}
}

func TestDenyStillWorksWithoutFrozenArgs(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted = r.Method == http.MethodPost
		fmt.Fprint(w, `{"status":"rejected"}`)
	}))
	defer srv.Close()
	m := newModel(Config{ServerURL: srv.URL})
	m.pending = []pendingApproval{{id: "a", tool: "mcp_pages_deploy_page"}}
	cmd := m.decideApproval([]string{"/deny"}, false)
	if cmd == nil {
		t.Fatal("deny should not require a frozen snapshot")
	}
	m.finishApproval(cmd().(approvalResolvedMsg))
	if !posted {
		t.Fatal("deny did not POST")
	}
}

func TestApproveShowsEditedExecutionArgs(t *testing.T) {
	name, prompt, cron := "new-name", "calculate 385", "0 9 * * *"
	card := withFrozen(pendingApproval{
		id: "a", tool: "schedule_task",
		edits: &ScheduleEdits{Name: &name, Prompt: &prompt, Cron: &cron},
	}, map[string]any{"name": "old", "prompt": "old prompt", "cron": "0 8 * * *"})
	review := frozenApprovalReview(card)
	if !strings.Contains(review, "new-name") || !strings.Contains(review, "calculate 385") || !strings.Contains(review, "0 9 * * *") {
		t.Fatal(review)
	}
	if strings.Contains(review, "old prompt") {
		t.Fatal("review showed staged args instead of local edits", review)
	}
}

func TestTUIPresentsFrozenArgsForEveryStagedTool(t *testing.T) {
	hidden := "git push origin main"
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "b",
		"tool":        "bash",
		"summary":     map[string]any{"command": strings.Repeat("echo safe; ", 20)},
		"frozen_args": map[string]any{"complete": true, "args": map[string]any{"command": strings.Repeat("echo safe; ", 20) + hidden}},
	}})
	if !strings.Contains(strings.Join(m.approvalLines, "\n"), hidden) {
		t.Fatal("TUI did not present the full frozen command")
	}

	m = newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id": "g",
		"tool":        "mcp_pages_deploy_page",
		"summary":     map[string]any{"args": "truncated"},
		"frozen_args": map[string]any{"complete": true, "args": map[string]any{"html": "VISIBLE_GENERIC_TAIL"}},
	}})
	if !strings.Contains(strings.Join(m.approvalLines, "\n"), "VISIBLE_GENERIC_TAIL") {
		t.Fatal("TUI omitted generic frozen args")
	}
}

func TestFinishApprovalSanitizesResultAndErrors(t *testing.T) {
	m := newModel(Config{})
	m.finishApproval(approvalResolvedMsg{approved: true, status: "approved", tool: "mcp_x", resultText: "ok\x1b]52;c;evil\x07\u202e"})
	got := m.history[len(m.history)-1]
	if strings.Contains(got, "\x1b]52") || strings.ContainsRune(got, '\x07') || strings.ContainsRune(got, '\u202e') {
		t.Fatal("result leaked terminal controls", got)
	}
	if !strings.Contains(got, `\u001b`) || !strings.Contains(got, `\u202e`) {
		t.Fatal("expected visible escapes", got)
	}
	m = newModel(Config{})
	m.finishApproval(approvalResolvedMsg{err: fmt.Errorf("fail\x1b[2J")})
	got = m.history[len(m.history)-1]
	if strings.Contains(got, "\x1b[2J") {
		t.Fatal("error leaked terminal controls", got)
	}
}

func TestCLISanitizesApprovalResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"pending_approvals": []any{map[string]any{"approval_id": "a", "tool": "bash", "frozen_args": map[string]any{"complete": true, "args": map[string]any{"command": "ls"}}}}})
			return
		}
		fmt.Fprint(w, `{"status":"approved","result_text":"done\u001b]52;c;evil"}`)
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	if code := runResolveApproval(NewClient(Config{ServerURL: srv.URL}), "c", "a", true, &out, &errOut); code != 0 {
		t.Fatal(code, errOut.String())
	}
	if strings.ContainsAny(out.String(), "\x1b") {
		t.Fatal(out.String())
	}
}

func TestParseFrozenArgsTypedNil(t *testing.T) {
	if args, complete, present := parseFrozenArgs(nil); present || complete || args != nil {
		t.Fatal("nil payload must be absent")
	}
	if args, complete, present := parseFrozenArgs(map[string]any(nil)); present || complete || args != nil {
		t.Fatal("typed-nil map must be absent")
	}
	if args, complete, present := parseFrozenArgs(json.RawMessage(nil)); present || complete || args != nil {
		t.Fatal("typed-nil RawMessage must be absent")
	}
	if _, complete, present := parseFrozenArgs(map[string]any{"complete": true}); !present || complete {
		t.Fatal("complete without args is incomplete")
	}
	if _, complete, present := parseFrozenArgs(map[string]any{"complete": true, "args": nil}); !present || complete {
		t.Fatal("typed-nil args is incomplete")
	}
}

func TestFrozenArgsPreserveJSONNumberLiteralsOnWire(t *testing.T) {
	const big = "9007199254740993"
	payload := `{"complete":true,"args":{"nested":{"id":` + big + `},"n":` + big + `,"rate":1e2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"pending_approvals":[{"approval_id":"a","tool":"mcp_pages_deploy_page","frozen_args":%s}]}`, payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	pending, err := NewClient(Config{ServerURL: srv.URL}).loadApprovals(context.Background(), "c")
	if err != nil || len(pending) != 1 {
		t.Fatalf("load: %v %v", pending, err)
	}
	review := frozenApprovalReview(pending[0])
	if !strings.Contains(review, big) || strings.Contains(review, "9007199254740992") {
		t.Fatalf("GET review rounded integer: %s", review)
	}
	if !strings.Contains(review, "1e2") {
		t.Fatalf("GET review lost exponent: %s", review)
	}

	var ev Event
	stream := "event: tool.approval_required\ndata: {\"approval_id\":\"s\",\"tool\":\"mcp_pages_deploy_page\",\"frozen_args\":" + payload + "}\n\n"
	if err := parseSSE(strings.NewReader(stream), func(e Event) { ev = e }); err != nil {
		t.Fatal(err)
	}
	card := pendingApproval{id: "s", tool: "mcp_pages_deploy_page"}
	card.frozenArgs, card.frozenComplete, card.frozenPresent = parseFrozenArgs(ev.Data["frozen_args"])
	review = frozenApprovalReview(card)
	if !strings.Contains(review, big) || strings.Contains(review, "9007199254740992") {
		t.Fatalf("SSE review rounded integer: %s", review)
	}
	if !strings.Contains(review, "1e2") {
		t.Fatalf("SSE review lost exponent: %s", review)
	}
}
