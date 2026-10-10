package httpapi

// Resume after approval (docs/RESUME-AFTER-APPROVAL.md), end to end against
// the real Postgres store and the real agent loop: every turn below is
// Manager.RunTurn against the fake LLM (internal/fakellm), and the requests
// the fake receives are recorded, so "the result is in context" is checked on
// the bytes the model was sent.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/fakellm"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

const (
	resumeTestUser  = "alice@x.com"
	resumeTestModel = "anthropic/claude-opus-4.8"
	resumeToolA     = "mcp_deals_update_deal"
	resumeToolB     = "mcp_deals_create_deal"
	resumeReply     = "Verified: the change is in place."
	resumeSlowReply = "Long answer to the user's question."
)

// llmRequest is one completion request the fake LLM received.
type llmRequest struct {
	body       string
	start, end time.Time
}

// llmRecorder sits in front of the fake LLM and keeps every request body.
type llmRecorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []llmRequest
}

func (r *llmRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		r.next.ServeHTTP(w, req)
		return
	}
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	start := time.Now()
	r.next.ServeHTTP(w, req)
	r.mu.Lock()
	r.reqs = append(r.reqs, llmRequest{body: string(body), start: start, end: time.Now()})
	r.mu.Unlock()
}

func (r *llmRecorder) requests() []llmRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llmRequest(nil), r.reqs...)
}

// lastUserText is the text of the request's last user message other than the
// loop's own trailing "## Runtime today" block — i.e. the input of the turn
// that sent it (the JSON-escaped body is decoded first).
func (q llmRequest) lastUserText() string {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(q.body), &body) != nil {
		return ""
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role != "user" {
			continue
		}
		text := messageContentText(body.Messages[i].Content)
		if strings.HasPrefix(text, "## Runtime today") {
			continue
		}
		return text
	}
	return ""
}

func messageContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// resumeEngine is the real agent.Manager with the approval path's MCP call
// seam swapped for a recording broker (the approved call itself is not under
// test; the turn after it is).
type resumeEngine struct {
	*agent.Manager
	broker  agentcore.MCPBroker
	catalog []mcp.ServerTool
}

func (e *resumeEngine) MCPBroker() agentcore.MCPBroker { return e.broker }
func (e *resumeEngine) MCPCatalog() []mcp.ServerTool   { return e.catalog }

// newResumeManager builds a real Manager wired to the fake LLM through h
// (the same seam internal/agent's fake-LLM tests use: OPENROUTER_BASE_URL,
// host-mode sandbox, a minimal prompt bundle).
func newResumeManager(t *testing.T, h http.Handler) *agent.Manager {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")
	dir := t.TempDir()
	workspaceRoot := filepath.Join(dir, "workspace")
	t.Setenv("FLEET_WORKSPACE_ROOT", workspaceRoot)
	for _, d := range []string{workspaceRoot, filepath.Join(dir, "protocols"), filepath.Join(dir, "system_prompts"), filepath.Join(dir, "personas")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "system_prompts", "chat.md"), []byte("# Test system prompt\n\nBe brief.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "personas", "generic.yaml"), []byte("name: Generic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := agent.New(agent.ManagerOptions{
		Config: &config.Config{
			MockMode:         true, // host-mode sandbox pool for the Manager only
			OpenRouterAPIKey: "test-key",
			PersonaDefault:   "generic",
			WorkspaceRoot:    workspaceRoot,
		},
		PersonasDir:      filepath.Join(dir, "personas"),
		ProtocolsDir:     filepath.Join(dir, "protocols"),
		SkillsDir:        filepath.Join(dir, "skills"),
		SystemPromptsDir: filepath.Join(dir, "system_prompts"),
		Limiter:          admission.New(4, 1),
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return mgr
}

type resumeHarness struct {
	t         *testing.T
	s         *Server
	rec       *llmRecorder
	broker    *budgetBroker
	decisions chan store.ApprovalResumeResult
}

// newResumeHarness wires the server fixture to a real Manager + fake LLM,
// installs policy (zero = no bundle declaration) and shortens the debounce.
func newResumeHarness(t *testing.T, policy agentcore.AgentPolicy, debounce time.Duration) *resumeHarness {
	t.Helper()
	s := serverFixture(t)
	agentcore.ConfigureAgentPolicy(policy)
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	prev := approvalResumeDebounce
	approvalResumeDebounce = debounce
	t.Cleanup(func() { approvalResumeDebounce = prev })

	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{fakellm.TextStep(resumeReply)}})
	fake.Scenario("slow", fakellm.Scenario{Steps: []fakellm.Step{{Kind: fakellm.StepText, Text: resumeSlowReply, Delay: 1500 * time.Millisecond}}})
	rec := &llmRecorder{next: fake.Handler()}
	broker := &budgetBroker{text: "deal D1 updated: floor 2.50"}
	s.agent = &resumeEngine{
		Manager: newResumeManager(t, rec),
		broker:  broker,
		catalog: []mcp.ServerTool{
			{ServerName: "deals", Tool: mcp.Tool{Name: "update_deal"}},
			{ServerName: "deals", Tool: mcp.Tool{Name: "create_deal"}},
		},
	}
	h := &resumeHarness{t: t, s: s, rec: rec, broker: broker, decisions: make(chan store.ApprovalResumeResult, 16)}
	s.approvalResumeObserver = func(_ string, res store.ApprovalResumeResult, err error) {
		if err != nil {
			t.Errorf("approval resume decision failed: %v", err)
		}
		h.decisions <- res
	}
	return h
}

func resumePolicy(maxPerHour int) agentcore.AgentPolicy {
	return agentcore.AgentPolicy{
		CriticalToolSuffixes:         []string{"update_deal", "create_deal"},
		CriticalToolResume:           []string{"update_deal", "create_deal"},
		CriticalToolResumeMaxPerHour: maxPerHour,
	}
}

func (h *resumeHarness) conversation() *store.Conversation {
	h.t.Helper()
	conv, err := h.s.store.CreateConversation(context.Background(), resumeTestUser, "deals", "generic", resumeTestModel, false)
	if err != nil {
		h.t.Fatal(err)
	}
	return conv
}

// stage records what a turn that staged a card leaves behind — the user's
// request, the tool call, the APPROVAL_REQUIRED placeholder and the promise
// to verify — and stages the card through the real stager (which arms it
// when the tool opted in).
func (h *resumeHarness) stage(convID, tool, callID string) *store.Approval {
	h.t.Helper()
	ctx := context.Background()
	entries := []agent.HistoryEntry{
		histEntry("user", "text", agent.TextContent{Text: "Update the deal floor to 2.50."}),
		histEntry("assistant", "tool_call", agent.ToolCallContent{ID: callID, Name: tool, Input: `{"deal_id":"D1"}`}),
		histEntry("tool", "tool_result", agent.ToolResultContent{ID: callID, Name: tool, Text: "APPROVAL_REQUIRED: waiting for the user"}),
		histEntry("assistant", "text", agent.TextContent{Text: "I staged the change. After approval I'll verify it."}),
	}
	if _, err := h.s.store.AppendHistory(ctx, convID, entries); err != nil {
		h.t.Fatal(err)
	}
	stager := &approvalStager{ctx: ctx, store: h.s.store, conversationID: convID, userEmail: resumeTestUser, sink: discardSink{}}
	id, err := stager.Stage(tool, callID, `{"deal_id":"D1"}`)
	if err != nil {
		h.t.Fatalf("Stage: %v", err)
	}
	a, err := h.s.store.GetApproval(ctx, resumeTestUser, id)
	if err != nil || a == nil {
		h.t.Fatalf("GetApproval: %v %v", a, err)
	}
	return a
}

func histEntry(role, typ string, content any) agent.HistoryEntry {
	raw, _ := json.Marshal(content)
	return agent.HistoryEntry{Role: role, Type: typ, Content: raw}
}

type discardSink struct{}

func (discardSink) Emit(string, any) {}

// resolve POSTs a decision and returns the decoded answer.
func (h *resumeHarness) resolve(convID, approvalID string, approved bool) map[string]any {
	h.t.Helper()
	w := do(h.t, h.s.Routes(), http.MethodPost, "/conversations/"+convID+"/approvals/"+approvalID,
		map[string]any{"approved": approved, "scope": "once"}, resumeTestUser)
	if w.Code != http.StatusOK {
		h.t.Fatalf("approval POST: %d %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// decision waits for the next resume decision the server made.
func (h *resumeHarness) decision() store.ApprovalResumeResult {
	h.t.Helper()
	select {
	case res := <-h.decisions:
		return res
	case <-time.After(waitForBudget):
		h.t.Fatal("no resume decision was made")
		return store.ApprovalResumeResult{}
	}
}

// idle waits until no turn is running in the conversation and the queue
// holds nothing still to run.
func (h *resumeHarness) idle(convID string) {
	h.t.Helper()
	waitFor(h.t, "the conversation to go idle", func() bool {
		if e, ok := h.s.getInflight(convID); ok && e.IsRunning() {
			return false
		}
		items, err := h.s.store.ListQueuedInputs(context.Background(), resumeTestUser, convID)
		return err == nil && len(items) == 0
	})
}

// resumeInputs returns the turn inputs fleet wrote itself, in order.
func (h *resumeHarness) resumeInputs(convID string) []agent.TextContent {
	h.t.Helper()
	hist, err := h.s.store.LoadHistory(context.Background(), convID)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []agent.TextContent
	for _, e := range hist {
		if e.Role != "user" || e.Type != "text" {
			continue
		}
		var c agent.TextContent
		_ = json.Unmarshal(e.Content, &c)
		if c.Kind == agent.InputKindApprovalResume {
			out = append(out, c)
		}
	}
	return out
}

// resumeRequests returns the LLM requests whose turn input is a resume input.
func (h *resumeHarness) resumeRequests() []llmRequest {
	var out []llmRequest
	for _, q := range h.rec.requests() {
		if strings.HasPrefix(q.lastUserText(), "[Approval") {
			out = append(out, q)
		}
	}
	return out
}

// Approve → the call runs, its result lands in history, and ONE turn starts
// on its own: the model receives the result and the labelled resume input,
// the input is persisted as fleet's (not the user's), and the reply is
// committed after it.
func TestApprovalResume_ApproveStartsATurnWithTheResultInContext(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	if a.ResumeState != store.ApprovalResumeArmed {
		t.Fatalf("staged card resume_state = %q, want armed", a.ResumeState)
	}

	out := h.resolve(conv.ID, a.ID, true)
	if out["status"] != "approved" || out["resume"] != true || out["is_err"] != false {
		t.Fatalf("approve answer = %v, want approved with resume:true", out)
	}
	res := h.decision()
	if res.Input == nil || len(res.Claimed) != 1 || res.Claimed[0].Outcome != store.ApprovalResumeOutcomeApproved {
		t.Fatalf("decision = %+v, want one resume input for the approved card", res)
	}
	h.idle(conv.ID)

	reqs := h.resumeRequests()
	if len(reqs) != 1 || len(h.rec.requests()) != 1 {
		t.Fatalf("LLM requests: %d resume of %d total, want exactly one (the resume turn)", len(reqs), len(h.rec.requests()))
	}
	for _, want := range []string{"deal D1 updated: floor 2.50", "outcome=approved", a.ID, resumeToolA} {
		if !strings.Contains(reqs[0].body, want) {
			t.Errorf("the resume turn's model request lacks %q", want)
		}
	}
	inputs := h.resumeInputs(conv.ID)
	if len(inputs) != 1 || !strings.HasPrefix(inputs[0].Text, "[Approval resolved] "+resumeToolA) {
		t.Fatalf("persisted resume inputs = %+v", inputs)
	}
	hist, _ := h.s.store.LoadHistory(context.Background(), conv.ID)
	sawInput, replied := false, false
	for _, e := range hist {
		var c agent.TextContent
		if e.Type != "text" || json.Unmarshal(e.Content, &c) != nil {
			continue
		}
		if e.Role == "user" && c.Kind == agent.InputKindApprovalResume {
			sawInput = true
		}
		if sawInput && e.Role == "assistant" && c.Text == resumeReply {
			replied = true
		}
	}
	if !replied {
		t.Fatal("the resume turn's reply was not committed after its input")
	}
	if got, _ := h.s.store.GetApproval(context.Background(), resumeTestUser, a.ID); got.ResumeState != store.ApprovalResumeClaimed {
		t.Fatalf("resume_state after the turn = %q, want claimed", got.ResumeState)
	}
	// The turn's live frames name it as fleet's: turn.started input_kind and
	// user.message kind, persisted in the turn ledger like every frame.
	events := h.turnEventsOfLastTurn(conv.ID)
	for _, want := range []string{`"input_kind":"approval_resume"`, `"kind":"approval_resume"`} {
		if !strings.Contains(events, want) {
			t.Errorf("the resume turn's stream lacks %s", want)
		}
	}
}

func (h *resumeHarness) turnEventsOfLastTurn(convID string) string {
	h.t.Helper()
	st := h.s.concreteStore(h.t)
	page, _, err := st.GetTurnEventPage(context.Background(), convID, 0, 500, true)
	if err != nil {
		h.t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range page {
		b.WriteString(e.Name)
		b.WriteString(" ")
		b.Write(e.Data)
		b.WriteString("\n")
	}
	return b.String()
}

// Decline → a turn starts too: the model reads that the action was not taken
// (and is told not to retry it), and nothing was executed.
func TestApprovalResume_DeclineStartsATurn(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	if out := h.resolve(conv.ID, a.ID, false); out["status"] != "rejected" || out["resume"] != true {
		t.Fatalf("decline answer = %v", out)
	}
	if res := h.decision(); res.Input == nil {
		t.Fatalf("decision = %+v, want a resume input", res)
	}
	h.idle(conv.ID)
	reqs := h.resumeRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d resume requests, want 1", len(reqs))
	}
	for _, want := range []string{"User declined the " + resumeToolA + " call", "outcome=declined", "Do not retry a declined"} {
		if !strings.Contains(reqs[0].body, want) {
			t.Errorf("the resume turn's model request lacks %q", want)
		}
	}
	if calls, _, _ := h.broker.snapshot(); calls != 0 {
		t.Fatalf("a declined card executed %d call(s)", calls)
	}
}

// Timeout → the expiry sweep auto-denies the card and that settlement starts
// a turn as well.
func TestApprovalResume_TimeoutStartsATurn(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 30*time.Millisecond)
	conv := h.conversation()
	ctx := context.Background()
	if _, err := h.s.store.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		histEntry("assistant", "tool_call", agent.ToolCallContent{ID: "call_t", Name: resumeToolA, Input: `{}`}),
		histEntry("tool", "tool_result", agent.ToolResultContent{ID: "call_t", Name: resumeToolA, Text: "APPROVAL_REQUIRED"}),
	}); err != nil {
		t.Fatal(err)
	}
	a, err := h.s.store.CreateApproval(ctx, conv.ID, resumeTestUser, resumeToolA, "call_t", `{}`, time.Now().Unix()-5, store.ApprovalSeat{})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.s.store.ArmApprovalResume(ctx, resumeTestUser, a.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n, err := h.s.SweepExpiredApprovals(ctx); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if res := h.decision(); res.Input == nil || res.Claimed[0].Outcome != store.ApprovalResumeOutcomeTimedOut {
		t.Fatalf("decision = %+v, want a resume for the timed-out card", res)
	}
	h.idle(conv.ID)
	reqs := h.resumeRequests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].body, "Approval timed out") || !strings.Contains(reqs[0].body, "outcome=timed_out") {
		t.Fatalf("resume requests = %d, want one carrying the timeout", len(reqs))
	}
}

// Two cards settled close together start ONE turn, after both: settling the
// first while the second is pending only defers.
func TestApprovalResume_TwoCardsCoalesceIntoOneTurn(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_a")
	b := h.stage(conv.ID, resumeToolB, "call_b")

	h.resolve(conv.ID, a.ID, true)
	if res := h.decision(); len(res.Outstanding) != 1 || res.Outstanding[0] != b.ID || res.Input != nil {
		t.Fatalf("first decision = %+v, want deferred on the pending card", res)
	}
	if n := len(h.rec.requests()); n != 0 {
		t.Fatalf("a turn started while a card was still pending (%d requests)", n)
	}
	h.resolve(conv.ID, b.ID, false)
	res := h.decision()
	if res.Input == nil || len(res.Claimed) != 2 {
		t.Fatalf("second decision = %+v, want one input for both cards", res)
	}
	h.idle(conv.ID)
	reqs := h.resumeRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d resume turns, want exactly 1", len(reqs))
	}
	in := reqs[0].lastUserText()
	if !strings.Contains(in, a.ID+" outcome=approved") || !strings.Contains(in, b.ID+" outcome=declined") {
		t.Fatalf("the coalesced input must name both cards: %q", in)
	}
}

// A turn already running: the resume waits in the queue (an 'auto-continue'
// row) and starts after that turn ends — never beside it.
func TestApprovalResume_RunningTurnQueuesTheResume(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")

	go postChatJSON(t, h.s, resumeTestUser, map[string]any{"conversation_id": conv.ID, "message": "[[scenario:slow]] while you wait, summarise the plan"})
	waitFor(t, "the user's turn to start", func() bool {
		e, ok := h.s.getInflight(conv.ID)
		return ok && e.IsRunning()
	})
	h.resolve(conv.ID, a.ID, true)
	res := h.decision()
	if res.Input == nil {
		t.Fatalf("decision = %+v, want a queued resume", res)
	}
	items, err := h.s.store.ListQueuedInputs(context.Background(), resumeTestUser, conv.ID)
	if err != nil || len(items) != 1 || items[0].Mode != store.InputModeResume || items[0].State != store.InputStateQueued {
		t.Fatalf("queue = %+v, %v; want one queued resume row while the turn runs", items, err)
	}
	if e, ok := h.s.getInflight(conv.ID); !ok || !e.IsRunning() {
		t.Fatal("the user's turn must still be running when the resume is queued")
	}
	h.idle(conv.ID)
	all := h.rec.requests()
	resumes := h.resumeRequests()
	if len(all) != 2 || len(resumes) != 1 {
		t.Fatalf("requests = %d (resume %d), want the user's turn then the resume turn", len(all), len(resumes))
	}
	if !strings.Contains(all[0].lastUserText(), "summarise the plan") || resumes[0].start.Before(all[0].end) {
		t.Fatal("the resume turn must start after the running turn ended")
	}
}

// The hourly cap: once the conversation has used it, a settled card starts no
// turn; the conversation gets a note instead, and the card says so.
func TestApprovalResume_HourlyCapSkipsWithANote(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(1), 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	h.resolve(conv.ID, a.ID, true)
	if res := h.decision(); res.Input == nil {
		t.Fatalf("first decision = %+v, want a resume", res)
	}
	h.idle(conv.ID)

	b := h.stage(conv.ID, resumeToolB, "call_2")
	h.resolve(conv.ID, b.ID, true)
	res := h.decision()
	if res.Input != nil || res.Skipped != store.ApprovalResumeSkipRateLimited {
		t.Fatalf("second decision = %+v, want rate_limited", res)
	}
	if n := len(h.resumeRequests()); n != 1 {
		t.Fatalf("%d resume turns ran, want 1 (the cap is 1 an hour)", n)
	}
	if e, ok := h.s.getInflight(conv.ID); ok && e.IsRunning() {
		t.Fatal("a turn started over the cap")
	}
	hist, _ := h.s.store.LoadHistory(context.Background(), conv.ID)
	if last := hist[len(hist)-1]; last.Type != agent.EntryTypeNotice || !strings.Contains(string(last.Content), "1 times in the last hour") {
		t.Fatalf("last history entry = %s %s, want the skip note", last.Type, last.Content)
	}
}

// Without critical_tool_resume nothing changes: the approve answer has
// exactly the old keys, the card is not armed, no decision is ever
// scheduled and no turn starts. With the key, a card of a tool that did not
// opt in starts nothing either.
func TestApprovalResume_NotOptedInChangesNothing(t *testing.T) {
	h := newResumeHarness(t, agentcore.AgentPolicy{CriticalToolSuffixes: []string{"update_deal"}}, 30*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	if a.ResumeState != "" {
		t.Fatalf("resume_state = %q, want empty without the bundle key", a.ResumeState)
	}
	out := h.resolve(conv.ID, a.ID, true)
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	if len(out) != 3 || out["status"] != "approved" || out["result_text"] != "deal D1 updated: floor 2.50" || out["is_err"] != false {
		t.Fatalf("approve answer = %v (keys %v), want exactly status/result_text/is_err", out, keys)
	}
	h.s.approvalResumes.mu.Lock()
	scheduled := len(h.s.approvalResumes.latest)
	h.s.approvalResumes.mu.Unlock()
	if scheduled != 0 {
		t.Fatal("a resume decision was scheduled with no tool opted in")
	}
	if fields := approvalClientFields(resumeToolA, `{}`, conv.ID); fields["resume_after_approval"] != nil {
		t.Fatalf("card payload gained resume_after_approval without the key: %v", fields)
	}

	// Opted in for create_deal only: an update_deal card resumes nothing.
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{"update_deal", "create_deal"},
		CriticalToolResume:   []string{"create_deal"},
	})
	c := h.stage(conv.ID, resumeToolA, "call_2")
	if out := h.resolve(conv.ID, c.ID, true); out["resume"] != nil {
		t.Fatalf("a card of a tool that did not opt in answered resume: %v", out)
	}
	if res := h.decision(); len(res.Claimed) != 0 || res.Input != nil {
		t.Fatalf("decision = %+v, want nothing claimed", res)
	}
	h.idle(conv.ID)
	if n := len(h.rec.requests()); n != 0 {
		t.Fatalf("%d LLM requests, want none", n)
	}
}

// A conversation deleted before the decision is never resumed, whether the
// deployment hard-deletes (the default) or soft-deletes conversations.
func TestApprovalResume_DeletedConversationIsNotResumed(t *testing.T) {
	for _, soft := range []bool{false, true} {
		t.Run(map[bool]string{false: "hard delete", true: "soft delete"}[soft], func(t *testing.T) {
			testDeletedConversationIsNotResumed(t, soft)
		})
	}
}

func testDeletedConversationIsNotResumed(t *testing.T, soft bool) {
	h := newResumeHarness(t, resumePolicy(0), 300*time.Millisecond)
	h.s.concreteStore(t).SetSoftDelete(soft)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	h.resolve(conv.ID, a.ID, true)
	if err := h.s.concreteStore(t).Delete(context.Background(), resumeTestUser, conv.ID); err != nil {
		t.Fatal(err)
	}
	if res := h.decision(); res.Input != nil || res.Skipped != store.ApprovalResumeSkipConversationGone {
		t.Fatalf("decision = %+v, want conversation_gone", res)
	}
	if n := len(h.rec.requests()); n != 0 {
		t.Fatalf("%d LLM requests for a deleted conversation", n)
	}
}

// Restart: a resume that was due when the process stopped (here the shutdown
// began during the debounce) is not run by the old process, and the next
// boot drops it with a note instead of starting it; a new process then has
// nothing to drain.
func TestApprovalResume_RestartDropsThePendingResumeWithANote(t *testing.T) {
	h := newResumeHarness(t, resumePolicy(0), 200*time.Millisecond)
	conv := h.conversation()
	a := h.stage(conv.ID, resumeToolA, "call_1")
	h.resolve(conv.ID, a.ID, true)
	h.s.shuttingDown.Store(true) // the drain began before the debounce fired
	time.Sleep(400 * time.Millisecond)
	select {
	case res := <-h.decisions:
		t.Fatalf("a draining process decided a resume: %+v", res)
	default:
	}
	if got, _ := h.s.store.GetApproval(context.Background(), resumeTestUser, a.ID); got.ResumeState != store.ApprovalResumeArmed {
		t.Fatalf("resume_state = %q, want still armed for boot recovery", got.ResumeState)
	}

	// Next boot.
	noted, err := h.s.concreteStore(t).DropApprovalResumesAtBoot(context.Background())
	if err != nil || noted != 1 {
		t.Fatalf("DropApprovalResumesAtBoot = %d, %v", noted, err)
	}
	h.s.shuttingDown.Store(false)
	h.s.maybeDrainQueue(conv.ID)
	if e, ok := h.s.getInflight(conv.ID); ok && e.IsRunning() {
		t.Fatal("the new process started the dropped resume")
	}
	if n := len(h.rec.requests()); n != 0 {
		t.Fatalf("%d LLM requests after restart, want none", n)
	}
	hist, _ := h.s.store.LoadHistory(context.Background(), conv.ID)
	if last := hist[len(hist)-1]; last.Type != agent.EntryTypeNotice || !strings.Contains(string(last.Content), "fleet restarted") {
		t.Fatalf("last entry = %s %s, want the restart note", last.Type, last.Content)
	}
}
