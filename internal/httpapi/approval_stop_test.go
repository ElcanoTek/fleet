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

func postStop(s *Server, conv string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/conversations/"+conv+"/approvals/ap1/stop", nil)
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

	rec := postStop(s, "c1")
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
			rec := postStop(s, tc.conv)
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
	s.armApprovalStop("ap1") // as decideApproval does before the claim
	for range 2 {
		rec := postStop(s, "c1")
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

// A synthetic-transport tool (inline http_tools, a2a peers) never offers
// Stop, whatever key matches it: its transport cannot cancel, so the card
// carries no stoppable key and the stop route refuses and records nothing.
func TestApprovalStop_NeverASyntheticTransportTool(t *testing.T) {
	stopPolicy(t)
	for _, tool := range []string{
		"mcp_" + mcp.HTTPToolServerName + "_execute_plan",
		"mcp_" + mcp.A2AToolServerName + "_execute_plan",
	} {
		if approvalStoppable(tool) {
			t.Errorf("%s: a synthetic-transport tool must never be stoppable", tool)
		}
		if _, ok := approvalClientFields(tool, `{}`, "c1")["stoppable"]; ok {
			t.Errorf("%s: its card must carry no stoppable key", tool)
		}
		a := stopRow()
		a.ToolName, a.Status, a.ResultText = tool, "approved", approvalExecutingSentinel
		st := &stopStore{approval: a}
		s := &Server{store: st}
		s.armApprovalStop("ap1")
		if rec := postStop(s, "c1"); rec.Code != 400 {
			t.Errorf("%s: stop = %d %s, want 400", tool, rec.Code, rec.Body.String())
		}
		if st.marks != 0 {
			t.Errorf("%s: no stop may be recorded", tool)
		}
	}
	if !approvalStoppable("mcp_ix_execute_plan") {
		t.Fatal("the same key must still offer Stop on an MCP tool")
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

// Accepting a stop and finalizing the outcome are ordered: a Stop accepted
// first is marked AND reported by finalize; once finalized, a Stop is refused
// and nothing is marked, so the row and the outcome's stop line agree. A
// running row with no armed signal is past finalizing too.
func TestApprovalStop_OrderedAgainstTheOutcome(t *testing.T) {
	stopPolicy(t)
	running := func() *stopStore {
		a := stopRow()
		a.Status, a.ResultText = "approved", approvalExecutingSentinel
		return &stopStore{approval: a}
	}

	st := running()
	s := &Server{store: st}
	s.armApprovalStop("ap1")
	if rec := postStop(s, "c1"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stopping":true`) {
		t.Fatalf("stop = %d %s", rec.Code, rec.Body.String())
	}
	if !s.finalizeApprovalStop("ap1") || st.marks != 1 {
		t.Fatalf("a stop accepted first must be marked (%d) and reported by finalize", st.marks)
	}

	st = running()
	s = &Server{store: st}
	s.armApprovalStop("ap1")
	if s.finalizeApprovalStop("ap1") {
		t.Fatal("no stop was asked for")
	}
	rec := postStop(s, "c1")
	if rec.Code != 200 || st.marks != 0 || strings.Contains(rec.Body.String(), `"stopping":true`) {
		t.Fatalf("a stop after finalizing = %d %s, marks %d; want refused and unmarked", rec.Code, rec.Body.String(), st.marks)
	}

	st = running()
	s = &Server{store: st}
	if rec := postStop(s, "c1"); rec.Code != 200 || st.marks != 0 {
		t.Fatalf("no armed signal: %d %s, marks %d; want no mark", rec.Code, rec.Body.String(), st.marks)
	}
}

// A settled stop stays visible after the tool leaves critical_tool_stop: it
// is history on the row. Offering Stop follows the current policy.
func TestWithApprovalStop_SettledStopOutlivesThePolicy(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"execute_plan"}})
	a := stopRow()
	a.Status, a.StopRequestedAt = "approved", 1
	settled := withApprovalStop(map[string]any{}, &a)
	if settled["stopped"] != true || settled["stoppable"] != nil {
		t.Fatalf("settled = %v, want stopped without stoppable", settled)
	}
	running := withApprovalStop(map[string]any{"executing": true}, &a)
	if running["stopping"] != nil || running["stoppable"] != nil {
		t.Fatalf("running = %v, want nothing offered without the policy", running)
	}
}

// A call stopped before it was sent is recorded with the before-send line,
// never the line that says the call ran and its server was asked to stop.
func TestExecuteClaimedApproval_StoppedBeforeSendSaysNothingRan(t *testing.T) {
	stopPolicy(t)
	a := stopRow()
	a.Status, a.ResultText = "approved", approvalExecutingSentinel
	st := &stopStore{approval: a, resulted: make(chan struct{})}
	child := &stopChild{}
	s := stopServerFor(t, st, child)
	s.armApprovalStop("ap1")
	s.approvalStopFor("ap1").stop()
	s.executeClaimedApproval(context.Background(), "u@example.com", "c1", "ap1", &a, approvalRequest{Approved: true})
	st.mu.Lock()
	defer st.mu.Unlock()
	if !strings.Contains(st.result, "before this call was sent") || strings.Contains(st.result, "while this call ran") || child.calls != 0 {
		t.Fatalf("result = %q, calls = %d; want the before-send line and no call", st.result, child.calls)
	}
}

// A stopped, narrowed call at the minimum output limit: the decision reserves
// room for the stop line, so a narrowing accepted for a stoppable tool keeps
// its exact line and the stop line whole and the whole result within the
// configured limit; one whose lines could not both fit is refused.
func TestNarrowing_ReservesRoomForTheStopLine(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolStop:           []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
	agentcore.SetMaxToolOutputBytes(agentcore.MinMaxToolOutputBytes)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	staged := make([]string, 0, 120)
	for i := range 120 {
		staged = append(staged, fmt.Sprintf("d%03d", i))
	}
	args, _ := json.Marshal(map[string]any{"deal_ids": staged})
	row := stopRow()
	row.ArgsJSON, row.NarrowableArg = string(args), "deal_ids"

	// 60 scattered positions: the line alone fits the room, but not beside a
	// stop line, so a stoppable tool refuses it.
	var scattered []string
	for i, v := range staged {
		if i%2 == 0 {
			scattered = append(scattered, v)
		}
	}
	n := &approvalNarrowing{Arg: "deal_ids", Staged: staged, Approved: scattered}
	if len(narrowingNote(n)) > agentcore.ApprovalTrailerRoom() {
		t.Fatalf("test setup: the line (%d) must fit the room (%d) alone", len(narrowingNote(n)), agentcore.ApprovalTrailerRoom())
	}
	if _, err := narrowApproval(&row, scattered, "deal_ids"); err == nil {
		t.Fatal("a narrowing whose line cannot fit beside the stop line must be refused for a stoppable tool")
	}

	got, err := narrowApproval(&row, staged[:60], "deal_ids")
	if err != nil {
		t.Fatalf("a contiguous selection: %v", err)
	}
	for _, stopLine := range []string{approvalStopNote, approvalStoppedBeforeSendNote} {
		trailer := approvalTrailer(got, stopLine)
		res, _, _ := governApprovalResultWithTrailer(context.Background(), &row, strings.Repeat("x", 64<<10), false, trailer)
		if !strings.HasSuffix(res, trailer) || len(res) > agentcore.MinMaxToolOutputBytes {
			t.Fatalf("result is %d bytes (want <= %d) or lost its lines", len(res), agentcore.MinMaxToolOutputBytes)
		}
	}
}

// The live output limit may be lowered while a narrowed, stoppable call
// runs. The narrowing is checked at decision against the room at the minimum
// limit, so the longest selection accepted under a roomy limit still settles
// within the limit live when it ends, with the narrowing and stop lines whole.
func TestNarrowing_StoppedResultFitsALimitLoweredWhileItRan(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolStop:           []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
	agentcore.SetMaxToolOutputBytes(64 << 10)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	staged := make([]string, 0, 500)
	for i := range 500 {
		staged = append(staged, fmt.Sprintf("d%03d", i))
	}
	args, _ := json.Marshal(map[string]any{"deal_ids": staged})
	row := stopRow()
	row.ArgsJSON, row.NarrowableArg = string(args), "deal_ids"

	// The longest line accepted at 64 KiB: leave out one more scattered
	// position at a time until the selection is refused.
	var n *approvalNarrowing
	for k := 1; ; k++ {
		leftOut := map[int]bool{}
		for j := range k {
			leftOut[300+2*j] = true
		}
		var approved []string
		for i, v := range staged {
			if !leftOut[i] {
				approved = append(approved, v)
			}
		}
		next, err := narrowApproval(&row, approved, "deal_ids")
		if err != nil {
			break
		}
		n = next
	}
	if n == nil {
		t.Fatal("test setup: no narrowing was accepted")
	}
	agentcore.SetMaxToolOutputBytes(agentcore.MinMaxToolOutputBytes) // lowered while the call ran
	for _, stopLine := range []string{approvalStopNote, approvalStoppedBeforeSendNote} {
		trailer := approvalTrailer(n, stopLine)
		res, _, _ := governApprovalResultWithTrailer(context.Background(), &row, strings.Repeat("x", 64<<10), false, trailer)
		if !strings.HasSuffix(res, trailer) {
			t.Fatalf("the lines were cut or changed: ...%q", res[max(0, len(res)-200):])
		}
		if len(res) > agentcore.MinMaxToolOutputBytes {
			t.Fatalf("result is %d bytes, over the %d-byte limit live at settlement", len(res), agentcore.MinMaxToolOutputBytes)
		}
	}
}

// closeHookEngine opens a per-approval scope whose Close runs onClose first:
// a scope teardown that takes a while, during which a Stop may arrive.
type closeHookEngine struct {
	*fakeEngine
	scope   agent.MCPScope
	onClose func()
}

func (e *closeHookEngine) MCPBroker() agentcore.MCPBroker { return nil }
func (e *closeHookEngine) MCPCatalog() []mcp.ServerTool   { return nil }
func (e *closeHookEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (e *closeHookEngine) OpenApprovalMCPScope(context.Context, agentcore.MCPSelection, string, agentcore.MCPUserIdentity) (*agent.MCPScope, error) {
	scope := e.scope
	scope.Close = func(context.Context) error {
		e.onClose()
		return nil
	}
	return &scope, nil
}

// The stop window closes as soon as the call returns, before its scope is
// torn down: a Stop pressed while the scope is still closing (up to
// approvalScopeCloseTimeout) is refused and never marked, so the outcome
// never claims a stop that no transport was listening for.
func TestRunStagedTool_ClosesTheStopWindowBeforeTheScope(t *testing.T) {
	stopPolicy(t)
	a := stopRow()
	a.MCPServer = "ix"
	running := a
	running.Status, running.ResultText = "approved", approvalExecutingSentinel
	st := &stopStore{approval: running}
	engine := &closeHookEngine{
		fakeEngine: &fakeEngine{},
		scope: agent.MCPScope{
			Broker:  &fakeMCPBroker{text: `{"success":true}`},
			Catalog: []mcp.ServerTool{{ServerName: "ix", Tool: mcp.Tool{Name: "execute_plan"}}},
		},
	}
	s := &Server{store: st, agent: engine}
	var during *httptest.ResponseRecorder
	engine.onClose = func() { during = postStop(s, "c1") }
	s.armApprovalStop(a.ID)

	text, err := s.runStagedTool(context.Background(), &a)
	if err != nil || text != `{"success":true}` {
		t.Fatalf("runStagedTool = %q, %v", text, err)
	}
	if during == nil {
		t.Fatal("test setup: the scope was not closed")
	}
	if during.Code != 200 || strings.Contains(during.Body.String(), `"stopping":true`) || st.marks != 0 {
		t.Fatalf("a Stop during the scope's close = %d %s, marks %d; want refused and unmarked", during.Code, during.Body.String(), st.marks)
	}
	if s.finalizeApprovalStop(a.ID) {
		t.Fatal("the outcome must not report a stop that arrived after the call returned")
	}
}

// openHookEngine runs onOpen while a per-approval scope opens, then fails the
// open with openErr, or opens a scope whose catalog lacks the staged tool.
type openHookEngine struct {
	*fakeEngine
	onOpen  func()
	openErr error
}

func (e *openHookEngine) MCPBroker() agentcore.MCPBroker { return nil }
func (e *openHookEngine) MCPCatalog() []mcp.ServerTool   { return nil }
func (e *openHookEngine) OpenApprovalRemoteMCPScope(context.Context, string, string, string) (*agent.RemoteMCPOverlay, error) {
	return nil, nil
}

func (e *openHookEngine) OpenApprovalMCPScope(context.Context, agentcore.MCPSelection, string, agentcore.MCPUserIdentity) (*agent.MCPScope, error) {
	e.onOpen()
	if e.openErr != nil {
		return nil, e.openErr
	}
	return &agent.MCPScope{
		Broker:  &fakeMCPBroker{text: `{"success":true}`},
		Catalog: []mcp.ServerTool{{ServerName: "ix", Tool: mcp.Tool{Name: "other_tool"}}},
		Close:   func(context.Context) error { return nil },
	}, nil
}

// A Stop accepted while the call's scope was still opening, followed by a
// failure before anything was dispatched (the scope would not open, or its
// catalog no longer has the tool), sent nothing and no cancellation: the
// outcome is "stopped before it was sent", so fleet's line says nothing ran,
// never that the server was asked to stop. Without a Stop the setup failure
// reads as before.
func TestExecuteClaimedApproval_SetupFailureAfterStopSaysNothingRan(t *testing.T) {
	stopPolicy(t)
	for name, openErr := range map[string]error{
		"the scope does not open":      errors.New("seat revoked"),
		"the tool is no longer listed": nil,
	} {
		for _, pressStop := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, stop=%t", name, pressStop), func(t *testing.T) {
				a := stopRow()
				a.MCPServer = "ix"
				running := a
				running.Status, running.ResultText = "approved", approvalExecutingSentinel
				st := &stopStore{approval: running}
				engine := &openHookEngine{fakeEngine: &fakeEngine{}, openErr: openErr}
				s := &Server{store: st, agent: engine}
				engine.onOpen = func() {
					if pressStop {
						if rec := postStop(s, "c1"); !strings.Contains(rec.Body.String(), `"stopping":true`) {
							t.Fatalf("stop while opening = %d %s", rec.Code, rec.Body.String())
						}
					}
				}
				s.armApprovalStop(a.ID)
				s.executeClaimedApproval(context.Background(), "u@example.com", "c1", a.ID, &a, approvalRequest{Approved: true, Scope: "once"})
				switch {
				case pressStop && (!strings.Contains(st.result, approvalStoppedBeforeSendNote) || strings.Contains(st.result, approvalStopNote)):
					t.Fatalf("result = %q, want the before-send line only", st.result)
				case pressStop && !strings.Contains(st.result, "nothing ran"):
					t.Fatalf("result = %q, want it to say nothing ran", st.result)
				case !pressStop && (strings.Contains(st.result, "[fleet]") || strings.Contains(st.result, "nothing ran")):
					t.Fatalf("result = %q, want the plain setup failure without a stop line", st.result)
				}
			})
		}
	}
}
