package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover stopping a running approved call
// (docs/APPROVAL-STOP.md): for a tool in critical_tool_stop, Stop records the
// stop on the executing row and closes the call's stop signal, which crosses
// the broker; the server's normal answer is recorded with fleet's line, and a
// Stop that lands before the call is sent runs nothing.

func stopPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{"execute_plan", "deploy_page"},
		CriticalToolStop:     []string{"execute_plan"},
	})
}

// stopStore is an approvals store for one row with the real claim and
// stop-mark discipline.
type stopStore struct {
	*store.Store
	mu       sync.Mutex
	approval store.Approval
	marks    int
	result   string
	resulted chan struct{}
}

func (s *stopStore) GetApproval(_ context.Context, user, id string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approval.ID != id || s.approval.UserEmail != user {
		return nil, nil
	}
	a := s.approval
	return &a, nil
}

func (s *stopStore) ClaimApproval(_ context.Context, _, _, status, text string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approval.Status != "pending" {
		return false, nil
	}
	s.approval.Status, s.approval.ResultText = status, text
	return true, nil
}

func (s *stopStore) MarkApprovalStopRequested(_ context.Context, user, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := &s.approval
	if a.ID != id || a.UserEmail != user || a.Status != "approved" || a.IsErr.Valid || a.ResultText != approvalExecutingSentinel || a.StopRequestedAt != 0 {
		return false, nil
	}
	s.marks++
	a.StopRequestedAt = time.Now().Unix()
	return true, nil
}

func (s *stopStore) SetApprovalResult(_ context.Context, _, _, text string, isErr bool) error {
	s.mu.Lock()
	s.result = text
	s.approval.ResultText = text
	s.approval.IsErr = sql.NullBool{Valid: true, Bool: isErr}
	s.mu.Unlock()
	if s.resulted != nil {
		close(s.resulted)
	}
	return nil
}

// stopChild answers once its call's stop signal closes, as a cooperative
// server does after its current item.
type stopChild struct {
	mcpbroker.Backend
	entered chan struct{}
	calls   int
	mu      sync.Mutex
}

func (c *stopChild) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	stop := mcp.StopSignal(ctx)
	if c.entered != nil {
		close(c.entered)
	}
	if stop == nil {
		return `{"success":true}`, false, nil
	}
	select {
	case <-stop:
		return `{"success":true,"stopped_reason":"stopped_by_user"}`, false, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
}

func stopServerFor(t *testing.T, st chatStore, child *stopChild) *Server {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mcpbroker.NewServer(child).Serve(ctx, serverConn)
	}()
	client := mcpbroker.NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close(); cancel(); <-done })
	return &Server{store: st, agent: &approvalEngine{
		fakeEngine: &fakeEngine{},
		broker:     client,
		catalog:    []mcp.ServerTool{{ServerName: "ix", Tool: mcp.Tool{Name: "execute_plan"}}, {ServerName: "pages", Tool: mcp.Tool{Name: "deploy_page"}}},
	}}
}

func stopRow() store.Approval {
	return store.Approval{
		ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_ix_execute_plan", ArgsJSON: `{"deal_ids":["a","b"]}`, Status: "pending",
	}
}

func postStop(s *Server, conv, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/conversations/"+conv+"/approvals/"+id+"/stop", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.conversationByID(rec, req)
	return rec
}

// The full path: the approve POST answers executing early, Stop records the
// stop and the server sees it over the broker, the call's normal answer is
// recorded with fleet's line, and the row reads stopped.
func TestApprovalStop_StopsARunningCallAndRecordsIt(t *testing.T) {
	stopPolicy(t)
	prev := approvalProgressEarlyReplyAfter
	approvalProgressEarlyReplyAfter = 50 * time.Millisecond
	t.Cleanup(func() { approvalProgressEarlyReplyAfter = prev })

	st := &stopStore{approval: stopRow(), resulted: make(chan struct{})}
	child := &stopChild{entered: make(chan struct{})}
	s := stopServerFor(t, st, child)

	body := postNarrowApproval(t, s, `{"approved":true,"scope":"once"}`)
	if body.Code != 200 || !strings.Contains(body.Body.String(), `"executing":true`) {
		t.Fatalf("approve = %d %s, want an early executing answer", body.Code, body.Body.String())
	}
	if !strings.Contains(body.Body.String(), `"stoppable":true`) {
		t.Fatalf("the executing answer must say the call can be stopped: %s", body.Body.String())
	}
	select {
	case <-child.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the server")
	}

	rec := postStop(s, "c1", "ap1")
	if rec.Code != 200 {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-st.resulted:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped call's outcome was never recorded")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.marks != 1 || st.approval.StopRequestedAt == 0 {
		t.Fatalf("stop marks = %d, at %d; want one durable mark", st.marks, st.approval.StopRequestedAt)
	}
	if !strings.HasPrefix(st.result, `{"success":true,"stopped_reason":"stopped_by_user"}`) || !strings.Contains(st.result, "pressed Stop") {
		t.Fatalf("recorded result = %q, want the server's answer then fleet's line", st.result)
	}
	if st.approval.IsErr.Bool {
		t.Fatal("a cooperative stop is a recorded success, not a failure")
	}
	// The signal is dropped when the execution goroutine ends, just after
	// the outcome write.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if s.approvalStopFor("ap1") == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stop signal must be dropped once the outcome is recorded")
		}
	}
	state := s.approvalClientState(&st.approval)
	if state["stopped"] != true || state["stoppable"] != true {
		t.Fatalf("settled state = %v, want stopped", state)
	}
}

// A Stop pressed after the claim but before the call is sent runs nothing.
func TestRunStagedTool_StoppedBeforeSendRunsNothing(t *testing.T) {
	stopPolicy(t)
	child := &stopChild{}
	s := stopServerFor(t, &stopStore{}, child)
	a := stopRow()
	s.armApprovalStop(a.ID)
	s.approvalStopFor(a.ID).stop()
	if _, err := s.runStagedTool(context.Background(), &a); err == nil || !strings.Contains(err.Error(), "nothing ran") {
		t.Fatalf("runStagedTool = %v, want stopped before send", err)
	}
	if child.calls != 0 {
		t.Fatalf("the server got %d calls, want none", child.calls)
	}
	// A tool that did not opt in carries no signal even when armed.
	s.armApprovalStop("ap2")
	s.approvalStopFor("ap2").stop()
	if text, err := s.runStagedTool(context.Background(), &store.Approval{ID: "ap2", ToolName: "mcp_pages_deploy_page", ArgsJSON: `{}`}); err != nil || text != `{"success":true}` {
		t.Fatalf("not-opted-in tool = %q, %v", text, err)
	}
}

// Stop answers by the card's state: refused for a tool that did not opt in,
// for a pending card, and for another conversation; a settled card's outcome
// without a mark; a second Stop records nothing new.
func TestHandleApprovalStop_Refusals(t *testing.T) {
	stopPolicy(t)
	cases := []struct {
		name string
		row  func() store.Approval
		conv string
		code int
	}{
		{"not opted in", func() store.Approval {
			a := stopRow()
			a.ToolName, a.Status, a.ResultText = "mcp_pages_deploy_page", "approved", approvalExecutingSentinel
			return a
		}, "c1", 400},
		{"pending", stopRow, "c1", 409},
		{"other conversation", func() store.Approval {
			a := stopRow()
			a.Status, a.ResultText = "approved", approvalExecutingSentinel
			return a
		}, "c2", 404},
		{"settled", func() store.Approval {
			a := stopRow()
			a.Status, a.ResultText, a.IsErr = "approved", "done", sql.NullBool{Valid: true}
			return a
		}, "c1", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stopStore{approval: tc.row()}
			s := &Server{store: st}
			rec := postStop(s, tc.conv, "ap1")
			if rec.Code != tc.code {
				t.Fatalf("stop = %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
			if st.marks != 0 {
				t.Fatal("no stop may be recorded")
			}
		})
	}

	// Executing: the first Stop marks once; a second answers stopping again
	// and marks nothing new.
	a := stopRow()
	a.Status, a.ResultText = "approved", approvalExecutingSentinel
	st := &stopStore{approval: a}
	s := &Server{store: st}
	for range 2 {
		rec := postStop(s, "c1", "ap1")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 200 || body["executing"] != true || body["stopping"] != true {
			t.Fatalf("stop = %d %v, want executing and stopping", rec.Code, body)
		}
	}
	if st.marks != 1 {
		t.Fatalf("marks = %d, want 1", st.marks)
	}
}

// The pending card offers Stop only for an opted-in tool.
func TestApprovalStopPayloads(t *testing.T) {
	stopPolicy(t)
	if approvalClientFields("mcp_ix_execute_plan", `{}`, "c1")["stoppable"] != true {
		t.Fatal("an opted-in tool's pending card must say it can be stopped")
	}
	if _, ok := approvalClientFields("mcp_pages_deploy_page", `{}`, "c1")["stoppable"]; ok {
		t.Fatal("another tool's card carries no stoppable key")
	}
}

// armCheckStore fails the test if the claim runs before the call's stop
// signal exists: a Stop that lands right after the claim must find it.
type armCheckStore struct {
	*stopStore
	s       *Server
	armedAt bool
}

func (a *armCheckStore) ClaimApproval(ctx context.Context, user, id, status, text string) (bool, error) {
	a.armedAt = a.s.approvalStopFor(id) != nil
	return a.stopStore.ClaimApproval(ctx, user, id, status, text)
}

// The stop signal is armed before the claim makes the row executing, and a
// request that loses the claim drops only its own reference.
func TestApprovalStop_ArmedBeforeTheClaim(t *testing.T) {
	stopPolicy(t)
	inner := &stopStore{approval: stopRow(), resulted: make(chan struct{})}
	st := &armCheckStore{stopStore: inner}
	child := &stopChild{}
	s := stopServerFor(t, st, child)
	st.s = s
	if rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once"}`); rec.Code != 200 {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if !st.armedAt {
		t.Fatal("the claim ran before the stop signal was armed")
	}

	// A losing request: the row is no longer pending, so its claim fails
	// and it must not drop a running winner's signal.
	winner := s.armApprovalStop("ap9")
	lost := &stopStore{approval: store.Approval{ID: "ap9", ConversationID: "c1", UserEmail: "u@example.com", ToolName: "mcp_ix_execute_plan", ArgsJSON: `{}`, Status: "pending"}}
	lost.approval.Status = "pending"
	s2 := &Server{store: &claimLosingStore{stopStore: lost}}
	s2.approvalStops.m = map[string]*approvalStopHandle{"ap9": winner}
	winner.refs = 1
	d := s2.decideApproval(context.Background(), nil, "u@example.com", "c1", "ap9", approvalRequest{Approved: true, Scope: "once"})
	if d.status != 0 && d.status != 409 {
		t.Fatalf("losing decision = %+v", d)
	}
	if s2.approvalStopFor("ap9") != winner {
		t.Fatal("a lost claim dropped the winner's stop signal")
	}
}

// claimLosingStore loses every claim, as when another request won it.
type claimLosingStore struct{ *stopStore }

func (c *claimLosingStore) ClaimApproval(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

// The registry shares one signal between references and forgets it with the
// last one.
func TestApprovalStopRegistry_RefCounts(t *testing.T) {
	s := &Server{}
	a := s.armApprovalStop("x")
	if b := s.armApprovalStop("x"); a != b {
		t.Fatal("a second arm must share the signal")
	}
	s.disarmApprovalStop("x")
	if s.approvalStopFor("x") != a {
		t.Fatal("dropping one of two references must keep the signal")
	}
	s.disarmApprovalStop("x")
	if s.approvalStopFor("x") != nil {
		t.Fatal("the last reference must forget the signal")
	}
	s.disarmApprovalStop("x") // no panic on an unknown id
}
