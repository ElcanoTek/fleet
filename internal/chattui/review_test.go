package chattui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApprovalOutcomesAreAuthoritative(t *testing.T) {
	for _, body := range []string{`{"status":"rejected","result_text":"expired"}`, `{"status":"approved","is_err":true,"result_text":"execution failed"}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"a","tool":"bash","frozen_args":{"complete":true,"args":{"command":"ls"}}}]}`)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer s.Close()
			var out, errOut bytes.Buffer
			if code := runResolveApproval(NewClient(Config{ServerURL: s.URL}), "c", "a", true, &out, &errOut); code != 1 {
				t.Fatalf("code %d; %s", code, out.String())
			}
			if out.Len() != 0 {
				t.Fatal("false success:", out.String())
			}
		})
	}
}

func TestResumeEditScopedApproval(t *testing.T) {
	var decision ApprovalDecision
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"a","tool":"schedule_task","summary":{"name":"old","prompt":"old prompt"},"pattern_args":{"name":"old","prompt":"old prompt"},"frozen_args":{"complete":true,"args":{"name":"old","prompt":"old prompt"}}}]}`)
			return
		}
		if r.URL.Path != "/conversations/other/approvals/a" {
			t.Errorf("wrong conversation: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&decision); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"status":"approved","result_text":"created"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	cmd := m.runSlash("/resume other")
	m.Update(cmd())
	if m.convID != "other" || len(m.pending) != 1 {
		t.Fatalf("resume: %+v", m.pending)
	}
	m.runSlash(`/edit {"name":"new","prompt":"calculate 385","cron":"0 9 * * *"}`)
	cmd = m.runSlash("/approve a pattern name=new")
	m.Update(cmd())
	if decision.Scope != "" || decision.Pattern != "" || decision.Edits == nil || *decision.Edits.Prompt != "calculate 385" {
		t.Fatalf("decision %+v", decision)
	}
	if len(m.pending) != 0 || m.reviewBusy {
		t.Fatal("not resolved")
	}
	got := m.cardPolicies["other"]["schedule_task"]
	if len(got) != 1 || got[0].Pattern != "name=new" {
		t.Fatalf("terminal session policy not retained: %+v", got)
	}
}

func TestFailedResolutionCanRetry(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "a", tool: "schedule_task"}, nil)}
	cmd := m.runSlash("/approve")
	m.Update(cmd())
	if len(m.pending) != 1 || m.pending[0].id != "a" || m.reviewBusy {
		t.Fatal("lost pending approval")
	}
}

func TestApprovalDeadlineAndPrefixedEmail(t *testing.T) {
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "expired", expiresAt: 10}, {id: "live", expiresAt: 30}}
	if !m.expireApprovals(time.Unix(20, 0)) {
		t.Fatal("expected local deadline change")
	}
	if len(m.pending) != 1 || m.pending[0].id != "live" {
		t.Fatal(m.pending)
	}
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "Local deadline passed") || !strings.Contains(joined, "server is authoritative") || !strings.Contains(joined, "/approvals reload") {
		t.Fatal(joined)
	}
	if m.expireApprovals(time.Unix(20, 0)) {
		t.Fatal("refresh only on change")
	}
	s := approvalSummaryLine("mcp_sendgrid_send_email", map[string]any{"to": "recipient@example.invalid", "subject": "Report", "content": strings.Repeat("x", 1000)})
	if !strings.Contains(s, "recipient@example.invalid") || !strings.Contains(s, "Report") {
		t.Fatal(s)
	}
}

func TestApprovalFlagsConflictBeforeConnection(t *testing.T) {
	if code := Run([]string{"--approve", "a", "--deny", "b"}); code != 2 {
		t.Fatal(code)
	}
}

func TestOneShotOmitsSupersededSettlement(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "event: conversation\ndata: {\"id\":\"c\"}\n\nevent: tool.approval_required\ndata: {\"tool\":\"schedule_task\",\"approval_id\":\"old\"}\n\nevent: tool.approval_superseded\ndata: {\"tool\":\"schedule_task\"}\n\nevent: tool.approval_required\ndata: {\"tool\":\"schedule_task\",\"approval_id\":\"new\"}\n\nevent: turn.completed\ndata: {}\n\n")
	}))
	defer s.Close()
	var out, errOut bytes.Buffer
	runOneShot(NewClient(Config{ServerURL: s.URL}), "", "schedule", strings.NewReader(""), &out, &errOut)
	if strings.Contains(errOut.String(), "--approve old") || !strings.Contains(errOut.String(), "--approve new") {
		t.Fatal(errOut.String())
	}
}

func TestApprovalRequestCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := NewClient(Config{ServerURL: s.URL}).ResolveApproval(ctx, "c", "a", true)
	if err == nil {
		t.Fatal("expected cancellation")
	}
}

func TestParallelToolResultsMatchCallIDs(t *testing.T) {
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"id": "a", "name": "first"}})
	m.applyEvent(Event{Name: "tool.call", Data: map[string]any{"id": "b", "name": "second"}})
	m.applyEvent(Event{Name: "tool.result", Data: map[string]any{"id": "a", "is_err": false}})
	if !strings.Contains(m.toolLines[0], "done") || !strings.Contains(m.toolLines[1], "running") {
		t.Fatal(m.toolLines)
	}
	m.applyEvent(Event{Name: "tool.result", Data: map[string]any{"id": "b", "is_err": true}})
	if !strings.Contains(m.toolLines[1], "failed") {
		t.Fatal(m.toolLines)
	}
}

func TestTerminalSessionResolvesNewScheduledCardsThroughEndpoint(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d ApprovalDecision
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			t.Error(err)
		}
		if d.Scope != "" || !d.Approved {
			t.Errorf("incorrect handler-only decision: %+v", d)
		}
		calls++
		fmt.Fprint(w, `{"status":"approved","result_text":"task created"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "first"
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "one", tool: "schedule_task"}, nil)}
	cmd := m.runSlash("/approve session")
	m.finishApproval(cmd().(approvalResolvedMsg))
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "two", tool: "schedule_task"}, nil)}
	cmd = m.autoResolveCard()
	if cmd == nil {
		t.Fatal("session did not resolve next card")
	}
	m.finishApproval(cmd().(approvalResolvedMsg))
	if calls != 2 {
		t.Fatal(calls)
	}
	m.convID = "different"
	m.pending = []pendingApproval{{id: "three", tool: "schedule_task"}}
	if m.autoResolveCard() != nil {
		t.Fatal("session authorized another conversation")
	}
}

func TestCardPoliciesAccumulateAndDenyWins(t *testing.T) {
	var approved []bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d ApprovalDecision
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			t.Error(err)
		}
		approved = append(approved, d.Approved)
		if d.Approved {
			fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
			return
		}
		fmt.Fprint(w, `{"status":"rejected","result_text":"denied"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {
			{Approved: true, Scope: "session"},
			{Approved: false, Scope: "pattern", Pattern: "name=blocked"},
		}},
	}
	m.pending = []pendingApproval{{id: "blocked", tool: "schedule_task", patternArgs: map[string]string{"name": "blocked"}}}
	cmd := m.autoResolveCard()
	if cmd == nil {
		t.Fatal("deny pattern should match")
	}
	msg := cmd().(approvalResolvedMsg)
	if msg.approved {
		t.Fatal("deny must win over session approve")
	}
	m.finishApproval(msg)
	if len(approved) != 1 || approved[0] {
		t.Fatalf("wire decision: %v", approved)
	}
}

func TestPatternMatchUsesFilepathAndRawStringsOnly(t *testing.T) {
	m := newModel(Config{})
	m.convID = "c"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {{Approved: true, Scope: "pattern", Pattern: "name=night*"}}},
	}
	card := pendingApproval{id: "a", tool: "schedule_task", patternArgs: map[string]string{"name": "nightly"}}
	if _, ok := m.matchCardPolicy(card); !ok {
		t.Fatal("filepath glob should match")
	}
	card.patternArgs = map[string]string{"name": "daily"}
	if _, ok := m.matchCardPolicy(card); ok {
		t.Fatal("non-matching glob")
	}
}

func TestOldServerWithoutPatternArgsDoesNotMatchSummary(t *testing.T) {
	m := newModel(Config{})
	m.convID = "c"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {{Approved: true, Scope: "pattern", Pattern: "name=new"}}},
	}
	m.pending = []pendingApproval{{id: "a", tool: "schedule_task", details: map[string]any{"name": "new"}}}
	if m.autoResolveCard() != nil {
		t.Fatal("summary keys must not substitute for pattern_args")
	}
	d, ok := m.matchCardPolicy(m.pending[0])
	if ok {
		t.Fatalf("matched without pattern_args: %+v", d)
	}
}

func TestPatternMatchesMergedLocalEdits(t *testing.T) {
	m := newModel(Config{})
	m.convID = "c"
	name := "new"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {{Approved: true, Scope: "pattern", Pattern: "name=new"}}},
	}
	card := pendingApproval{id: "a", tool: "schedule_task", patternArgs: map[string]string{"name": "old"}, edits: &ScheduleEdits{Name: &name}}
	if _, ok := m.matchCardPolicy(card); !ok {
		t.Fatal("edited name should be the matching string")
	}
	card.edits = nil
	if _, ok := m.matchCardPolicy(card); ok {
		t.Fatal("unstaged original name must not match name=new")
	}
}

func TestPatternJoinPreservesSpaces(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "a", tool: "schedule_task", patternArgs: map[string]string{"name": "foo bar"}}, nil)}
	cmd := m.runSlash("/approve a pattern name=foo bar")
	if cmd == nil {
		t.Fatal("pattern with spaces rejected")
	}
	msg := cmd().(approvalResolvedMsg)
	m.finishApproval(msg)
	got := m.cardPolicies["c"]["schedule_task"]
	if len(got) != 1 || got[0].Pattern != "name=foo bar" {
		t.Fatalf("pattern %+v", got)
	}
}

func TestHandlerPatternKeyUnavailable(t *testing.T) {
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "b", tool: "schedule_task"}}
	m.runSlash("/approve pattern name=x")
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "no handler pattern_args") {
		t.Fatal(joined)
	}
	if len(m.pending) != 1 {
		t.Fatal("old server must not settle on a guessed pattern")
	}
}

func TestOptionalPatternKeyRegistersForFutureCards(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "a", tool: "schedule_task", patternArgs: map[string]string{"name": "x"}}, nil)}
	cmd := m.runSlash("/approve pattern cron=0 9 * * *")
	if cmd == nil {
		t.Fatal("optional cron pattern should register even when this card has no cron")
	}
	m.finishApproval(cmd().(approvalResolvedMsg))
	got := m.cardPolicies["c"]["schedule_task"]
	if len(got) != 1 || got[0].Pattern != "cron=0 9 * * *" {
		t.Fatalf("future optional pattern not retained: %+v", got)
	}
}

func TestShowApprovalsExposesPatternKeys(t *testing.T) {
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "a", tool: "schedule_task", summary: "nightly", patternArgs: map[string]string{"prompt": "p", "name": "n"}, details: map[string]any{"name": "n"}}}
	m.showApprovals()
	joined := strings.Join(m.history, "\n")
	if !strings.Contains(joined, "Pattern keys: name, prompt") {
		t.Fatal(joined)
	}
	m.pending = []pendingApproval{{id: "b", tool: "schedule_task"}}
	m.showApprovals()
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "server did not send pattern_args") {
		t.Fatal(joined)
	}
}

func TestAutoResolveOnReloadAndExpiredChain(t *testing.T) {
	ids := []string{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"live","tool":"schedule_task","pattern_args":{"name":"ok"},"frozen_args":{"complete":true,"args":{"name":"ok"}}},{"approval_id":"stale","tool":"schedule_task","expires_at":1,"pattern_args":{"name":"ok"},"frozen_args":{"complete":true,"args":{"name":"ok"}}}]}`)
			return
		}
		ids = append(ids, r.URL.Path)
		fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"other": {"schedule_task": {{Approved: true, Scope: "session"}}},
	}
	load := m.loadApprovals("other")
	msg := load().(approvalsLoadedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	_, cmd := m.Update(msg)
	if len(m.pending) != 0 || !m.reviewBusy {
		t.Fatalf("reload should drop expired card and dispatch the live card: %+v", m.pending)
	}
	if cmd == nil {
		t.Fatal("reload should auto-resolve remaining matching cards")
	}
	resolved := cmd().(approvalResolvedMsg)
	if resolved.card.id != "live" {
		t.Fatalf("expired card blocked the chain: %+v", resolved.card)
	}
	m.finishApproval(resolved)
	if len(ids) != 1 || !strings.HasSuffix(ids[0], "/approvals/live") {
		t.Fatal(ids)
	}
}

func TestAutoBlockedIsPerCardAndResetsOnRetry(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/approvals/bad") {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		calls++
		fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.statusErr = "unrelated turn error"
	m.cardPolicies = map[string]map[string][]ApprovalDecision{
		"c": {"schedule_task": {{Approved: true, Scope: "session"}}},
	}
	m.pending = []pendingApproval{
		withFrozen(pendingApproval{id: "bad", tool: "schedule_task"}, nil),
		withFrozen(pendingApproval{id: "good", tool: "schedule_task"}, nil),
	}
	cmd := m.autoResolveCard()
	if cmd == nil {
		t.Fatal("statusErr must not latch auto-resolve")
	}
	m.finishApproval(cmd().(approvalResolvedMsg))
	if len(m.pending) != 2 || !m.pending[0].autoBlocked || m.pending[0].id != "bad" {
		t.Fatalf("failed card should requeue blocked: %+v", m.pending)
	}
	cmd = m.autoResolveCard()
	if cmd == nil {
		t.Fatal("unrelated card should still resolve")
	}
	msg := cmd().(approvalResolvedMsg)
	if msg.card.id != "good" {
		t.Fatalf("expected good card, got %s", msg.card.id)
	}
	m.finishApproval(msg)
	if calls != 1 {
		t.Fatal(calls)
	}
	if m.autoResolveCard() != nil {
		t.Fatal("blocked card must not loop")
	}
	m.resetApprovalAutoBlocked()
	if m.pending[0].autoBlocked {
		t.Fatal("reload/retry should reset per-card block")
	}
}

func TestResumeDifferentConversationClearsRetry(t *testing.T) {
	m := newModel(Config{})
	m.convID = "first"
	m.lastUser = "old thread message"
	updated, _ := m.Update(approvalsLoadedMsg{conversation: "other"})
	m = updated.(*model)
	if m.lastUser != "" || m.convID != "other" {
		t.Fatalf("resume to another conversation must drop /retry: lastUser=%q conv=%q", m.lastUser, m.convID)
	}
	m.lastUser = "keep me"
	updated, _ = m.Update(approvalsLoadedMsg{conversation: "other"})
	m = updated.(*model)
	if m.lastUser != "keep me" {
		t.Fatal("same-conversation reload must preserve lastUser")
	}
	updated, _ = m.Update(approvalsLoadedMsg{conversation: "third", err: fmt.Errorf("nope")})
	m = updated.(*model)
	if m.lastUser != "keep me" || m.convID != "other" {
		t.Fatalf("failed resume must not switch threads: lastUser=%q conv=%q", m.lastUser, m.convID)
	}
}

func TestWelcomeAndNewDiscoverApprovals(t *testing.T) {
	m := newModel(Config{ServerURL: "http://x", Email: "e@x.co"})
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "/approvals") {
		t.Fatal("welcome should discover /approvals")
	}
	m.runSlash("/new")
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "/approvals") || !strings.Contains(joined, "new conversation") {
		t.Fatal(joined)
	}
	m.runSlash("/help")
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "raw handler string fields") {
		t.Fatal(joined)
	}
}

func TestLoadApprovalsParsesPatternArgsFromGET(t *testing.T) {
	var gotURL string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RawQuery
		fmt.Fprint(w, `{"pending_approvals":[{"approval_id":"a","tool":"schedule_task","summary":{"name":"n","prompt_preview":"truncated"},"pattern_args":{"name":"n","prompt":"full prompt","cron":"0 9 * * *","run_at":""},"frozen_args":{"complete":true,"args":{"name":"n","prompt":"full prompt","cron":"0 9 * * *"}}}]}`)
	}))
	defer s.Close()
	pending, err := NewClient(Config{ServerURL: s.URL}).loadApprovals(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	if pending[0].patternArgs["prompt"] != "full prompt" || pending[0].patternArgs["name"] != "n" || pending[0].patternArgs["cron"] != "0 9 * * *" {
		t.Fatalf("pattern_args %+v", pending[0].patternArgs)
	}
	if !pending[0].reviewComplete() || pending[0].frozenArgs["prompt"] != "full prompt" {
		t.Fatalf("frozen_args %+v complete=%v", pending[0].frozenArgs, pending[0].reviewComplete())
	}
	if _, ok := pending[0].patternArgs["prompt_preview"]; ok {
		t.Fatal("display summary keys must not appear as pattern_args")
	}
	if gotURL != "approval_id=a&omit_history=1&settlement_only=1" {
		t.Fatalf("loadApprovals query = %q, want omit_history=1 so settlement does not download the transcript", gotURL)
	}
}

func TestEditRejectsCronOnOneTimeScheduleCard(t *testing.T) {
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "a", tool: "schedule_task", details: map[string]any{"run_at": "2026-09-19T00:00:00Z", "recurring": false}}}
	m.editApproval(`/edit {"cron":"0 9 * * *"}`)
	if m.pending[0].edits != nil {
		t.Fatalf("cron edit applied to a one-time card: %+v", m.pending[0].edits)
	}
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "recurring") {
		t.Fatalf("expected rejection note, got %q", joined)
	}
}

func TestEditRejectsClearingRecurringCron(t *testing.T) {
	m := newModel(Config{})
	m.pending = []pendingApproval{{id: "a", tool: "schedule_task", details: map[string]any{"cron": "0 9 * * *", "recurring": true}}}
	m.editApproval(`/edit {"cron":""}`)
	if m.pending[0].edits != nil {
		t.Fatalf("empty cron applied to a recurring card: %+v", m.pending[0].edits)
	}
	if joined := strings.Join(m.history, "\n"); !strings.Contains(joined, "immediate") {
		t.Fatalf("expected rejection note, got %q", joined)
	}
}

func TestApplyEventParsesPatternArgs(t *testing.T) {
	m := newModel(Config{})
	m.applyEvent(Event{Name: "tool.approval_required", Data: map[string]any{
		"approval_id":  "a",
		"tool":         "schedule_task",
		"summary":      map[string]any{"name": "n", "prompt_preview": "trunc"},
		"pattern_args": map[string]any{"name": "n", "prompt": "full"},
		"frozen_args":  map[string]any{"complete": true, "args": map[string]any{"name": "n", "prompt": "full"}},
	}})
	if len(m.pending) != 1 || m.pending[0].patternArgs["prompt"] != "full" || !m.pending[0].reviewComplete() {
		t.Fatalf("%+v", m.pending)
	}
}

func TestPoliciesAccumulatePerTool(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"approved","result_text":"ok"}`)
	}))
	defer s.Close()
	m := newModel(Config{ServerURL: s.URL})
	m.convID = "c"
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "one", tool: "schedule_task", patternArgs: map[string]string{"name": "a"}}, nil)}
	m.finishApproval(m.runSlash("/approve session")().(approvalResolvedMsg))
	m.pending = []pendingApproval{withFrozen(pendingApproval{id: "two", tool: "schedule_task", patternArgs: map[string]string{"name": "b"}}, nil)}
	m.finishApproval(m.runSlash("/approve pattern name=b")().(approvalResolvedMsg))
	got := m.cardPolicies["c"]["schedule_task"]
	if len(got) != 2 || got[0].Scope != "session" || got[1].Pattern != "name=b" {
		t.Fatalf("accumulated %+v", got)
	}
}
