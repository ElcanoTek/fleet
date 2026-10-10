package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover the approved-call budget (docs/APPROVED-CALL-BUDGET.md):
// the budget an approved card's MCP call runs under, that it crosses the
// broker, and that a long call on a server that opted in answers the POST
// with "executing" and finishes detached, recording its outcome exactly once.

// useApprovedCallPolicy installs a bundle policy declaring deals_mcp's
// approved-call budget (and nothing else) for one test.
func useApprovedCallPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ApprovedCallTimeoutSeconds: map[string]int{"deals_mcp": 300},
	})
}

// useEarlyReplyAfter shortens the early-reply threshold for one test.
func useEarlyReplyAfter(t *testing.T, d time.Duration) {
	t.Helper()
	prev := approvalEarlyReplyAfter
	approvalEarlyReplyAfter = d
	t.Cleanup(func() { approvalEarlyReplyAfter = prev })
}

// budgetBroker records the budget and deadline the approved call carried,
// and can hold the call until the test releases it.
type budgetBroker struct {
	mu       sync.Mutex
	calls    int
	budget   time.Duration
	deadline time.Duration
	entered  chan struct{} // closed on the first call, if set
	release  chan struct{} // the call blocks until closed, if set
	text     string
}

func (b *budgetBroker) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	if d, ok := mcp.CallTimeout(ctx); ok {
		b.budget = d
	}
	if dl, ok := ctx.Deadline(); ok {
		b.deadline = time.Until(dl)
	}
	b.mu.Unlock()
	if first && b.entered != nil {
		close(b.entered)
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	return b.text, false, nil
}

func (b *budgetBroker) snapshot() (calls int, budget, deadline time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.budget, b.deadline
}

// budgetEngine reopens a per-approval scope whose catalog carries the staged
// seat's registered server, and records the deadline the open ran under.
type budgetEngine struct {
	*fakeEngine
	broker      agentcore.MCPBroker
	catalog     []mcp.ServerTool
	openMu      sync.Mutex
	openDL      time.Duration
	openCtxDone func() bool
}

func (e *budgetEngine) MCPBroker() agentcore.MCPBroker { return e.broker }
func (e *budgetEngine) MCPCatalog() []mcp.ServerTool   { return e.catalog }
func (e *budgetEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (e *budgetEngine) OpenApprovalMCPScope(ctx context.Context, _ agentcore.MCPSelection, _ string) (*agent.MCPScope, error) {
	e.openMu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		e.openDL = time.Until(dl)
	}
	e.openCtxDone = func() bool { return ctx.Err() != nil }
	e.openMu.Unlock()
	return &agent.MCPScope{Broker: e.broker, Catalog: e.catalog, Close: func(context.Context) error { return nil }}, nil
}

var dealsApprovalCatalog = []mcp.ServerTool{
	{ServerName: "deals_mcp_tunnl", Tool: mcp.Tool{Name: "create_deal"}},
	{ServerName: "send_grid", Tool: mcp.Tool{Name: "send_email"}},
}

func dealsApproval(args string) *store.Approval {
	return &store.Approval{
		ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_deals_mcp_tunnl_create_deal", ArgsJSON: args,
		MCPServer: "deals_mcp", MCPAccount: "tunnl", Status: "pending",
	}
}

func dealIDsArgs(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = `"D` + strings.Repeat("1", i+1) + `"`
	}
	return `{"deal_ids":[` + strings.Join(ids, ",") + `]}`
}

// TestRunStagedTool_ApprovedCallBudget: the call runs under the budget the
// server declared (scaled for deal_ids), attaches it as the per-call budget
// the broker carries, and runs the scope open under its own fixed allowance.
// A server that declared nothing keeps the flat 60 s.
func TestRunStagedTool_ApprovedCallBudget(t *testing.T) {
	useApprovedCallPolicy(t)
	cases := []struct {
		name     string
		approval *store.Approval
		want     time.Duration
	}{
		{"declared server: its budget", dealsApproval(`{"name":"one"}`), 300 * time.Second},
		// 59 × the default 20 s batch pace = 1180 s, above the 300 s base.
		{"declared server: deal_ids scaling", dealsApproval(dealIDsArgs(59)), 1180 * time.Second},
		{"undeclared server: the flat 60 s", &store.Approval{
			ToolName: "mcp_send_grid_send_email", ArgsJSON: dealIDsArgs(59),
			MCPServer: "send_grid",
		}, 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := &budgetBroker{text: "ok"}
			engine := &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}
			s := &Server{agent: engine}
			if _, err := s.runStagedTool(context.Background(), tc.approval); err != nil {
				t.Fatalf("runStagedTool: %v", err)
			}
			_, budget, deadline := broker.snapshot()
			if budget != tc.want {
				t.Fatalf("per-call budget (mcp.WithCallTimeout) = %v, want %v", budget, tc.want)
			}
			if deadline <= tc.want-time.Minute || deadline > tc.want {
				t.Fatalf("call deadline = %v left, want ~%v", deadline, tc.want)
			}
			engine.openMu.Lock()
			openDL, openDone := engine.openDL, engine.openCtxDone
			engine.openMu.Unlock()
			if openDL <= 0 || openDL > approvalScopeOpenTimeout {
				t.Fatalf("scope open deadline = %v left, want its own allowance of at most %v", openDL, approvalScopeOpenTimeout)
			}
			if openDone == nil || !openDone() {
				t.Fatal("the scope-open context must be released once the scope is open, not held for the call")
			}
		})
	}
}

// TestRunStagedTool_ApprovedCallBudgetCrossesTheBroker drives the call over
// the real broker protocol: the credential-owning side must see the budget
// as its per-call timeout (callTimeoutMs), or a server-side cut could fire
// at a default instead.
func TestRunStagedTool_ApprovedCallBudgetCrossesTheBroker(t *testing.T) {
	useApprovedCallPolicy(t)
	child := &budgetBroker{text: "created"}
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mcpbroker.NewServer(&budgetWireBackend{budgetBroker: child}).Serve(ctx, serverConn)
	}()
	client := mcpbroker.NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close(); cancel(); <-done })
	s := &Server{agent: &approvalEngine{fakeEngine: &fakeEngine{}, broker: client, catalog: dealsApprovalCatalog}}

	// A legacy row (no seat) runs on the shared broker; the budget follows the
	// catalog server's declaration all the same.
	text, err := s.runStagedTool(context.Background(), &store.Approval{
		ToolName: "mcp_deals_mcp_tunnl_create_deal", ArgsJSON: dealIDsArgs(59),
	})
	if err != nil || text != "created" {
		t.Fatalf("runStagedTool = %q, %v", text, err)
	}
	if _, budget, _ := child.snapshot(); budget != 1180*time.Second {
		t.Fatalf("budget seen across the broker = %v, want 1180s", budget)
	}
}

type budgetWireBackend struct {
	mcpbroker.Backend
	*budgetBroker
}

func (b *budgetWireBackend) CallMCP(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	return b.budgetBroker.CallMCP(ctx, server, tool, args)
}

// claimStore is an approvals store with the real claim discipline: one
// pending→approved winner, the executing sentinel until the outcome is
// written, and a counter of how many outcomes were written.
type claimStore struct {
	*store.Store
	mu        sync.Mutex
	approval  store.Approval
	claims    int
	results   int
	resultErr error
}

func (c *claimStore) GetApproval(context.Context, string, string) (*store.Approval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.approval
	return &a, nil
}

func (c *claimStore) ClaimApproval(_ context.Context, _, _, status, resultText string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.approval.Status != "pending" {
		return false, nil
	}
	c.claims++
	c.approval.Status, c.approval.ResultText = status, resultText
	return true, nil
}

func (c *claimStore) SetApprovalResult(_ context.Context, _, _, text string, isErr bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resultErr != nil {
		return c.resultErr
	}
	c.results++
	c.approval.ResultText = text
	c.approval.IsErr = sql.NullBool{Valid: true, Bool: isErr}
	return nil
}

func (c *claimStore) counts() (claims, results int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims, c.results
}

func postApproval(t *testing.T, s *Server) map[string]any {
	t.Helper()
	body, err := postApprovalRaw(s)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// postApprovalRaw is postApproval for a goroutine other than the test's own,
// which must not call t.Fatal.
func postApprovalRaw(s *Server) (map[string]any, error) {
	req := httptest.NewRequest("POST", "/conversations/c1/approvals/ap1", strings.NewReader(`{"approved":true,"scope":"once"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c1", "ap1")
	if rec.Code != 200 {
		return nil, fmt.Errorf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		return nil, fmt.Errorf("bad JSON %q: %w", rec.Body.String(), err)
	}
	return body, nil
}

// postExpectingEarlyReply POSTs the approval while broker holds the call.
// A POST that waited for the outcome would never answer, so it is bounded
// well below the test timeout and fails by name instead of hanging.
func postExpectingEarlyReply(t *testing.T, s *Server, broker *budgetBroker) map[string]any {
	t.Helper()
	replied := make(chan map[string]any, 1)
	go func() {
		body, err := postApprovalRaw(s)
		if err != nil {
			body = map[string]any{"error": err.Error()}
		}
		replied <- body
	}()
	select {
	case body := <-replied:
		return body
	case <-time.After(5 * time.Second):
		close(broker.release)
		<-replied
		t.Fatal("the POST was held open for the whole call; a declared server's long call must answer executing")
		return nil
	}
}

func drainApprovals(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.DrainApprovalRuns(ctx) {
		t.Fatal("approved execution never finished")
	}
}

// TestHandleApproval_LongDeclaredCallAnswersExecutingThenResult: a call on a
// server with a declared budget that is still running at the threshold
// answers "executing"; a "Check result" re-POST meanwhile reports executing
// and never re-runs it; once it finishes, its outcome is recorded once and the
// next re-POST returns it.
func TestHandleApproval_LongDeclaredCallAnswersExecutingThenResult(t *testing.T) {
	useApprovedCallPolicy(t)
	useEarlyReplyAfter(t, 10*time.Millisecond)
	broker := &budgetBroker{text: `{"success":true,"deal_id":"D1"}`, entered: make(chan struct{}), release: make(chan struct{})}
	st := &claimStore{approval: *dealsApproval(`{"name":"one"}`)}
	s := &Server{store: st, agent: &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}}

	first := postExpectingEarlyReply(t, s, broker)
	if first["status"] != "approved" || first["executing"] != true || first["is_err"] != nil {
		t.Fatalf("first POST = %v, want the executing shape while the call runs", first)
	}
	<-broker.entered

	check := postApproval(t, s)
	if check["executing"] != true || check["is_err"] != nil {
		t.Fatalf("Check result while running = %v, want executing", check)
	}

	close(broker.release)
	drainApprovals(t, s)

	final := postApproval(t, s)
	if final["status"] != "approved" || final["is_err"] != false || final["executing"] != nil {
		t.Fatalf("Check result after the call = %v, want the recorded success", final)
	}
	if !strings.Contains(final["result_text"].(string), `"deal_id":"D1"`) {
		t.Fatalf("result_text = %q, want the tool's output", final["result_text"])
	}
	calls, _, _ := broker.snapshot()
	claims, results := st.counts()
	if calls != 1 || claims != 1 || results != 1 {
		t.Fatalf("calls/claims/outcome writes = %d/%d/%d, want exactly one of each", calls, claims, results)
	}
}

// TestHandleApproval_DetachedCallPersistenceFailureIsUnknown: a detached
// call whose outcome cannot be written must not report success or keep
// reporting executing: the next Check result says the outcome is unknown.
func TestHandleApproval_DetachedCallPersistenceFailureIsUnknown(t *testing.T) {
	useApprovedCallPolicy(t)
	useEarlyReplyAfter(t, 10*time.Millisecond)
	broker := &budgetBroker{text: "created", entered: make(chan struct{}), release: make(chan struct{})}
	st := &claimStore{approval: *dealsApproval(`{}`), resultErr: errors.New("history unavailable")}
	s := &Server{store: st, agent: &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}}

	if first := postExpectingEarlyReply(t, s, broker); first["executing"] != true {
		t.Fatalf("first POST = %v, want executing", first)
	}
	<-broker.entered
	close(broker.release)
	drainApprovals(t, s)
	check := postApproval(t, s)
	if check["execution_unknown"] != true || check["executing"] != nil || check["is_err"] != nil {
		t.Fatalf("Check result after a failed outcome write = %v, want execution_unknown", check)
	}
}

// TestHandleApproval_FastDeclaredCallRepliesWithResult: a declared server's
// call that finishes before the threshold answers with its outcome, exactly
// as before.
func TestHandleApproval_FastDeclaredCallRepliesWithResult(t *testing.T) {
	useApprovedCallPolicy(t)
	useEarlyReplyAfter(t, time.Minute)
	broker := &budgetBroker{text: "created"}
	st := &claimStore{approval: *dealsApproval(`{}`)}
	s := &Server{store: st, agent: &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}}

	body := postApproval(t, s)
	if body["status"] != "approved" || body["is_err"] != false || body["result_text"] != "created" || body["executing"] != nil {
		t.Fatalf("POST = %v, want the recorded outcome", body)
	}
	if _, results := st.counts(); results != 1 {
		t.Fatalf("outcome writes = %d, want 1", results)
	}
}

// TestHandleApproval_UndeclaredServerWaitsForTheOutcome: a server that did
// not opt in keeps the request open until the outcome is recorded, however
// short the threshold, exactly as before the budget existed.
func TestHandleApproval_UndeclaredServerWaitsForTheOutcome(t *testing.T) {
	useApprovedCallPolicy(t)
	useEarlyReplyAfter(t, time.Millisecond)
	broker := &budgetBroker{text: "sent", entered: make(chan struct{}), release: make(chan struct{})}
	st := &claimStore{approval: store.Approval{
		ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_send_grid_send_email", ArgsJSON: `{}`, MCPServer: "send_grid", Status: "pending",
	}}
	s := &Server{store: st, agent: &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}}

	type result struct {
		body map[string]any
		err  error
	}
	replied := make(chan result, 1)
	go func() {
		body, err := postApprovalRaw(s)
		replied <- result{body, err}
	}()
	<-broker.entered
	// Far past the 1 ms threshold: an early reply would have arrived by now.
	select {
	case r := <-replied:
		t.Fatalf("an undeclared server's POST replied before its call returned: %v (%v)", r.body, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(broker.release)
	r := <-replied
	if r.err != nil {
		t.Fatal(r.err)
	}
	body := r.body
	if body["is_err"] != false || body["result_text"] != "sent" || body["executing"] != nil {
		t.Fatalf("POST = %v, want the recorded outcome", body)
	}
}

// TestApprovalMayAnswerEarly pins which approvals may reply before their
// outcome: only MCP calls on a server that declared a budget, found through
// the staged seat or, for a legacy row, the catalog.
func TestApprovalMayAnswerEarly(t *testing.T) {
	useApprovedCallPolicy(t)
	s := &Server{agent: &budgetEngine{fakeEngine: &fakeEngine{}, catalog: dealsApprovalCatalog}}
	cases := []struct {
		name string
		a    store.Approval
		want bool
	}{
		{"declared seat", *dealsApproval(`{}`), true},
		{"declared base seat", store.Approval{ToolName: "mcp_deals_mcp_create_deal", MCPServer: "deals_mcp"}, true},
		{"legacy row resolved through the catalog", store.Approval{ToolName: "mcp_deals_mcp_tunnl_create_deal"}, true},
		{"undeclared server", store.Approval{ToolName: "mcp_send_grid_send_email", MCPServer: "send_grid"}, false},
		{"legacy row not in the catalog", store.Approval{ToolName: "mcp_other_tool"}, false},
		{"bash", store.Approval{ToolName: "bash"}, false},
		{"schedule_task", store.Approval{ToolName: "schedule_task"}, false},
		{"preview_email", store.Approval{ToolName: "preview_email"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.approvalMayAnswerEarly(&tc.a); got != tc.want {
				t.Fatalf("approvalMayAnswerEarly = %v, want %v", got, tc.want)
			}
		})
	}
}
