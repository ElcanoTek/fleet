package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover approval narrowing (docs/APPROVAL-NARROWING.md,
// ADR-0085): a person approves only some of the staged values of a tool's
// declared list argument, and fleet runs the staged call with only that
// argument replaced.

func narrowPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan", "deploy_page"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
}

const narrowStagedArgs = `{"summary":"Create 3 deals · plan p1 r2","deal_ids":["a","b","c"],"values_file":"plans/p1.json","values_sha256":"abc","n":9007199254740993}`

func narrowApprovalRow() store.Approval {
	return store.Approval{
		ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_ix_execute_plan", ArgsJSON: narrowStagedArgs, Status: "pending",
	}
}

// narrowStore is an approvals store with the real claim discipline for one
// row, recording which claim approved it and what was narrowed.
type narrowStore struct {
	*store.Store
	mu             sync.Mutex
	approval       store.Approval
	wholeClaims    int
	narrowedClaims int
	result         string
}

func (n *narrowStore) GetApproval(context.Context, string, string) (*store.Approval, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a := n.approval
	return &a, nil
}

func (n *narrowStore) ClaimApproval(_ context.Context, _, _, status, text string) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.approval.Status != "pending" {
		return false, nil
	}
	n.wholeClaims++
	n.approval.Status, n.approval.ResultText = status, text
	return true, nil
}

func (n *narrowStore) ClaimApprovalNarrowed(_ context.Context, _, _, text, record string) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.approval.Status != "pending" {
		return false, nil
	}
	n.narrowedClaims++
	n.approval.Status, n.approval.ResultText, n.approval.NarrowedJSON = "approved", text, record
	return true, nil
}

func (n *narrowStore) SetApprovalResult(_ context.Context, _, _, text string, isErr bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.result = text
	n.approval.ResultText = text
	n.approval.IsErr = sql.NullBool{Valid: true, Bool: isErr}
	return nil
}

// narrowChild is the credential-owning side: it records the arguments and the
// fleet `_meta` the approved call reached it with.
type narrowChild struct {
	mcpbroker.Backend
	mu   sync.Mutex
	args map[string]any
	meta map[string]any
	tool string
}

func (c *narrowChild) CallMCP(ctx context.Context, _, tool string, args map[string]any) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args, c.meta, c.tool = args, mcp.RequestMeta(ctx), tool
	return `{"success":true}`, false, nil
}

// narrowServer wires a Server whose approved calls cross the real broker
// protocol to child.
func narrowServer(t *testing.T, st chatStore, child *narrowChild) *Server {
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
		catalog:    []mcp.ServerTool{{ServerName: "ix", Tool: mcp.Tool{Name: "execute_plan"}}},
	}}
}

func postNarrowApproval(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/conversations/c1/approvals/ap1", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c1", "ap1")
	return rec
}

// The person approves two of three staged deal_ids, out of order: the call
// runs with exactly those two, in staged order, every other argument as
// staged (a large integer included), and the server is told what was staged.
// The claim records the narrowing, and the result names what was left out.
func TestHandleApproval_NarrowedRunsOnlyTheApprovedValues(t *testing.T) {
	narrowPolicy(t)
	st := &narrowStore{approval: narrowApprovalRow()}
	child := &narrowChild{}
	s := narrowServer(t, st, child)

	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["c","a"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if st.narrowedClaims != 1 || st.wholeClaims != 0 {
		t.Fatalf("claims: narrowed %d, whole %d; want one narrowed claim", st.narrowedClaims, st.wholeClaims)
	}
	if got := child.args["deal_ids"]; !reflect.DeepEqual(got, []any{"a", "c"}) {
		t.Fatalf("deal_ids reached the server as %v, want [a c] (staged order)", got)
	}
	for _, k := range []string{"summary", "values_file", "values_sha256"} {
		var staged map[string]any
		_ = json.Unmarshal([]byte(narrowStagedArgs), &staged)
		if child.args[k] != staged[k] {
			t.Errorf("%s = %v, want the staged %v", k, child.args[k], staged[k])
		}
	}
	if child.args["n"] != json.Number("9007199254740993") {
		t.Errorf("a staged large integer changed: %v (%T)", child.args["n"], child.args["n"])
	}
	wantMeta := map[string]any{narrowedRequestMetaKey: map[string]any{"arg": "deal_ids", "staged": []any{"a", "b", "c"}}}
	if !reflect.DeepEqual(child.meta, wantMeta) {
		t.Fatalf("_meta = %#v, want %#v", child.meta, wantMeta)
	}
	if st.approval.NarrowedJSON != `{"arg":"deal_ids","staged":["a","b","c"],"approved":["a","c"]}` {
		t.Fatalf("narrowing record = %s", st.approval.NarrowedJSON)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	narrowed, _ := body["narrowed"].(map[string]any)
	if narrowed == nil || !reflect.DeepEqual(narrowed["approved"], []any{"a", "c"}) {
		t.Fatalf("reply narrowed = %v", body["narrowed"])
	}
	if !strings.HasPrefix(st.result, `{"success":true}`) || !strings.Contains(st.result, `of the 3 staged deal_ids in its arguments, only positions 2 were left out; the other 2 were approved. Only the approved ones could be sent to the tool`) {
		t.Fatalf("recorded result = %q, want the tool's text then fleet's note", st.result)
	}
}

// Approving every staged value is an ordinary whole approval: the plain claim,
// the staged arguments, no `_meta`, no record.
func TestHandleApproval_ApprovingEveryValueIsAWholeApproval(t *testing.T) {
	narrowPolicy(t)
	st := &narrowStore{approval: narrowApprovalRow()}
	child := &narrowChild{}
	s := narrowServer(t, st, child)
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["b","c","a"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if st.wholeClaims != 1 || st.narrowedClaims != 0 || st.approval.NarrowedJSON != "" {
		t.Fatalf("claims: whole %d narrowed %d record %q", st.wholeClaims, st.narrowedClaims, st.approval.NarrowedJSON)
	}
	if !reflect.DeepEqual(child.args["deal_ids"], []any{"a", "b", "c"}) || child.meta != nil {
		t.Fatalf("args %v meta %v, want the staged call unchanged", child.args["deal_ids"], child.meta)
	}
}

// Every malformed narrowing is refused before a claim: the card stays
// pending and nothing runs.
func TestHandleApproval_RefusesABadNarrowingBeforeTheClaim(t *testing.T) {
	cases := []struct {
		name, body, args string
		policy           bool
	}{
		{name: "empty", body: `{"approved":true,"approved_values":[]}`, policy: true},
		{name: "explicit null", body: `{"approved":true,"approved_values":null}`, policy: true},
		{name: "unknown value", body: `{"approved":true,"approved_values":["a","z"]}`, policy: true},
		{name: "repeated value", body: `{"approved":true,"approved_values":["a","a"]}`, policy: true},
		{name: "with a decline", body: `{"approved":false,"approved_values":["a"]}`, policy: true},
		{name: "with a session scope", body: `{"approved":true,"scope":"session","approved_values":["a"]}`, policy: true},
		{name: "tool declares nothing", body: `{"approved":true,"approved_values":["a"]}`},
		{name: "one staged value", body: `{"approved":true,"approved_values":["a"]}`, args: `{"deal_ids":["a"]}`, policy: true},
		{name: "staged list repeats a value", body: `{"approved":true,"approved_values":["a"]}`, args: `{"deal_ids":["a","a","b"]}`, policy: true},
		{name: "staged list holds a number", body: `{"approved":true,"approved_values":["a"]}`, args: `{"deal_ids":["a",7]}`, policy: true},
		{name: "staged value too long for a row key", body: `{"approved":true,"approved_values":["a"]}`, args: `{"deal_ids":["a","` + strings.Repeat("x", 201) + `"]}`, policy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.policy {
				narrowPolicy(t)
			} else {
				t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
				agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"execute_plan"}})
			}
			row := narrowApprovalRow()
			if tc.args != "" {
				row.ArgsJSON = tc.args
			}
			st := &narrowStore{approval: row}
			child := &narrowChild{}
			s := narrowServer(t, st, child)
			rec := postNarrowApproval(t, s, tc.body)
			if rec.Code != 400 {
				t.Fatalf("POST = %d %s, want 400", rec.Code, rec.Body.String())
			}
			if st.wholeClaims+st.narrowedClaims != 0 || st.approval.Status != "pending" || child.tool != "" {
				t.Fatalf("a refused narrowing claimed or ran something: %+v", st.approval)
			}
		})
	}
}

// Execution re-checks the record against the staged call: one that does not
// match refuses the run instead of running the whole staged list.
func TestApprovalExecutionArgs_RefusesAMismatchedRecord(t *testing.T) {
	a := narrowApprovalRow()
	a.NarrowedJSON = `{"arg":"deal_ids","staged":["a","b","c"],"approved":["c"]}`
	args, n, err := approvalExecutionArgs(&a)
	if err != nil || n == nil || !reflect.DeepEqual(args["deal_ids"], []any{"c"}) {
		t.Fatalf("a matching record: args %v, n %v, err %v", args["deal_ids"], n, err)
	}
	for _, record := range []string{
		`{"arg":"deal_ids","staged":["a","b","x"],"approved":["a"]}`,         // staged differs
		`{"arg":"deal_ids","staged":["a","b","c"],"approved":["c","a"]}`,     // out of order
		`{"arg":"deal_ids","staged":["a","b","c"],"approved":["z"]}`,         // not staged
		`{"arg":"deal_ids","staged":["a","b","c"],"approved":["a","b","c"]}`, // not a narrowing
		`{"arg":"summary","staged":["a","b","c"],"approved":["a"]}`,          // another argument
		`{"arg":"deal_ids","staged":["a","b","c"],"approved":["a"],"x":1}`,   // unknown key
		`not json`,
	} {
		a.NarrowedJSON = record
		if _, _, err := approvalExecutionArgs(&a); err == nil || !strings.Contains(err.Error(), "nothing was run") {
			t.Errorf("record %s: err = %v, want a refusal", record, err)
		}
	}
	a.NarrowedJSON = ""
	if args, n, err := approvalExecutionArgs(&a); err != nil || n != nil || !reflect.DeepEqual(args["deal_ids"], []any{"a", "b", "c"}) {
		t.Fatalf("no record: args %v n %v err %v", args["deal_ids"], n, err)
	}
}

// The pending card offers the narrowable values only for a declared tool
// whose staged value is a list of distinct strings; a settled card carries
// what was approved.
func TestNarrowingPayloads(t *testing.T) {
	narrowPolicy(t)
	fields := approvalClientFields("mcp_ix_execute_plan", narrowStagedArgs, "c1")
	want := map[string]any{"arg": "deal_ids", "values": []string{"a", "b", "c"}}
	if !reflect.DeepEqual(fields["narrowable"], want) {
		t.Fatalf("narrowable = %#v, want %#v", fields["narrowable"], want)
	}
	for _, tc := range []struct{ tool, args string }{
		{"mcp_pages_deploy_page", `{"deal_ids":["a","b"]}`},
		{"mcp_ix_execute_plan", `{"deal_ids":["a"]}`},
		{"mcp_ix_execute_plan", `{"deal_ids":"a,b"}`},
		{"mcp_ix_execute_plan", `{}`},
	} {
		if _, ok := approvalClientFields(tc.tool, tc.args, "c1")["narrowable"]; ok {
			t.Errorf("%s %s: narrowable present, want absent", tc.tool, tc.args)
		}
	}

	s := &Server{}
	row := narrowApprovalRow()
	row.Status, row.ResultText, row.IsErr = "approved", "ok", sql.NullBool{Valid: true}
	if _, ok := s.approvalClientState(&row)["narrowed"]; ok {
		t.Fatal("a whole approval carries no narrowed record")
	}
	row.NarrowedJSON = `{"arg":"deal_ids","staged":["a","b","c"],"approved":["b"]}`
	got, _ := json.Marshal(s.approvalClientState(&row)["narrowed"])
	if string(got) != `{"arg":"deal_ids","staged":["a","b","c"],"approved":["b"]}` {
		t.Fatalf("narrowed = %s", got)
	}
}

// A describer card row may name a staged value with `key`; keys are unique
// and bounded like labels.
func TestParseApprovalCard_Keys(t *testing.T) {
	if _, canonical, err := parseApprovalCard([]byte(`{"title":"t","items":[{"key":"a","label":"A"},{"label":"B"}]}`)); err != nil || !strings.Contains(string(canonical), `"key":"a"`) {
		t.Fatalf("a keyed row: %s, %v", canonical, err)
	}
	for _, card := range []string{
		`{"title":"t","items":[{"key":"a","label":"A"},{"key":"a","label":"B"}]}`,
		`{"title":"t","items":[{"key":"` + strings.Repeat("k", 201) + `","label":"A"}]}`,
		`{"title":"t","items":[{"key":"a\u0007","label":"A"}]}`,
		`{"title":"t","items":[{"key":7,"label":"A"}]}`,
	} {
		if _, _, err := parseApprovalCard([]byte(card)); err == nil {
			t.Errorf("card %s accepted, want refused", card)
		}
	}
}

// A grouped decision narrows one card and approves another whole; a bad
// narrowing refuses the whole decision before any claim.
func TestApprovalGroup_Narrowing(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolGroupApproval:  []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
	rows := func() []store.Approval {
		r := groupRows()
		for i := range r {
			r[i].ArgsJSON = `{"deal_ids":["x","y","z"]}`
		}
		return r
	}
	post := func(st *narrowGroupStore, body string) *httptest.ResponseRecorder {
		s := &Server{store: st, cfg: &config.Config{MockMode: true}}
		req := httptest.NewRequest("POST", "/conversations/c1/approval-groups/turn-1", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
		rec := httptest.NewRecorder()
		s.handleApprovalGroup(rec, req, "c1", "turn-1")
		return rec
	}

	st := &narrowGroupStore{groupStore: newGroupStore(rows()...), narrowed: map[string]string{}}
	rec := post(st, `{"approve":["ap-pm","ap-mg"],"decline":["ap-ix"],"narrow":{"ap-pm":["z","x"]}}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if st.narrowed["ap-pm"] != `{"arg":"deal_ids","staged":["x","y","z"],"approved":["x","z"]}` {
		t.Fatalf("ap-pm record = %q", st.narrowed["ap-pm"])
	}
	if _, ok := st.narrowed["ap-mg"]; ok || st.claimCount("ap-mg") != 1 || st.state("ap-ix").Status != "rejected" {
		t.Fatalf("ap-mg must be approved whole and ap-ix declined: %v %d %s", st.narrowed, st.claimCount("ap-mg"), st.state("ap-ix").Status)
	}

	for _, body := range []string{
		`{"approve":["ap-pm"],"decline":["ap-mg"],"narrow":{"ap-mg":["x"]}}`,                                                                   // narrowing a declined card
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":["q"]}}`,                                                                               // not a staged value
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":[]}}`,                                                                                  // empty
		`{"approve":["ap-pm"],"narrow":{"ap-pm":null}}`,                                                                                        // null
		`{"approve":["ap-pm"],"narrow":{"ap-pm":[` + strings.TrimSuffix(strings.Repeat(`"x",`, maxApprovalGroupNarrowedValues+1), ",") + `]}}`, // over the group's value budget
	} {
		st := &narrowGroupStore{groupStore: newGroupStore(rows()...), narrowed: map[string]string{}}
		if rec := post(st, body); rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", body, rec.Code, rec.Body.String())
		}
		for _, id := range []string{"ap-pm", "ap-mg", "ap-ix"} {
			if st.claimCount(id) != 0 || len(st.narrowed) != 0 {
				t.Errorf("%s: %s was claimed by a refused decision", body, id)
			}
		}
	}
}

// narrowGroupStore adds the narrowed claim to groupStore.
type narrowGroupStore struct {
	*groupStore
	narrowed map[string]string
}

func (g *narrowGroupStore) ClaimApprovalNarrowed(ctx context.Context, user, id, text, record string) (bool, error) {
	ok, err := g.ClaimApproval(ctx, user, id, "approved", text)
	if ok {
		g.mu.Lock()
		g.narrowed[id] = record
		g.approvals[id].NarrowedJSON = record
		g.mu.Unlock()
	}
	return ok, err
}

// A long tool result is truncated to what the output bound leaves after
// fleet's narrowing line, which is never cut: the whole stays within the
// bound and ends with the complete line.
func TestGovernWithTrailer_KeepsTheWholeNoteWithinTheBound(t *testing.T) {
	narrowPolicy(t)
	agentcore.SetMaxToolOutputBytes(4096)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	row := narrowApprovalRow()
	staged := make([]string, 0, 500)
	approved := make([]string, 0, 250)
	for i := range 500 {
		v := fmt.Sprintf("deal-%03d", i)
		staged = append(staged, v)
		if i%2 == 0 {
			approved = append(approved, v)
		}
	}
	note := narrowingNote(&approvalNarrowing{Arg: "deal_ids", Staged: staged, Approved: approved})
	got, _, _ := governApprovalResultWithTrailer(context.Background(), &row, strings.Repeat("x", 64<<10), false, note)
	if !strings.HasSuffix(got, note) {
		t.Fatalf("the note was cut or changed: ...%q", got[max(0, len(got)-200):])
	}
	if len(got) > 4096 {
		t.Fatalf("result is %d bytes, over the 4096-byte bound", len(got))
	}
}

// blockingNarrowChild holds the approved call until released.
type blockingNarrowChild struct {
	mcpbroker.Backend
	release chan struct{}
}

func (c *blockingNarrowChild) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return `{"success":true}`, false, nil
}

// A narrowed call that answers "executing" early says what was approved in
// that answer too, so the running card keeps its narrowing.
func TestHandleApproval_EarlyExecutingAnswerCarriesTheNarrowing(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolProgress:       []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
	prev := approvalProgressEarlyReplyAfter
	approvalProgressEarlyReplyAfter = 20 * time.Millisecond
	t.Cleanup(func() { approvalProgressEarlyReplyAfter = prev })

	st := &narrowStore{approval: narrowApprovalRow()}
	child := &blockingNarrowChild{release: make(chan struct{})}
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mcpbroker.NewServer(child).Serve(ctx, serverConn)
	}()
	client := mcpbroker.NewClient(clientConn)
	t.Cleanup(func() { close(child.release); _ = client.Close(); cancel(); <-done })
	s := &Server{store: st, agent: &approvalEngine{
		fakeEngine: &fakeEngine{},
		broker:     client,
		catalog:    []mcp.ServerTool{{ServerName: "ix", Tool: mcp.Tool{Name: "execute_plan"}}},
	}}
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["a","c"]}`)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	narrowed, _ := body["narrowed"].(map[string]any)
	if rec.Code != 200 || body["executing"] != true || narrowed == nil || !reflect.DeepEqual(narrowed["approved"], []any{"a", "c"}) {
		t.Fatalf("early answer = %d %s, want executing with the narrowing", rec.Code, rec.Body.String())
	}
}

// The audit line names staged positions, never the left-out values.
func TestApprovalNarrowing_LeftOutPositions(t *testing.T) {
	n := &approvalNarrowing{Arg: "deal_ids", Staged: []string{"a", "secret-b", "c", "d", "e"}, Approved: []string{"a", "e"}}
	if got := n.leftOutPositions(); got != "2-4" {
		t.Fatalf("leftOutPositions = %q, want 2-4", got)
	}
}

// failingResultNarrowStore cannot record the outcome.
type failingResultNarrowStore struct{ *narrowStore }

func (f *failingResultNarrowStore) SetApprovalResult(context.Context, string, string, string, bool) error {
	return errors.New("db down")
}

// An outcome that could not be recorded still answers what was approved, so
// the card keeps its narrowing.
func TestHandleApproval_UnknownOutcomeKeepsTheNarrowing(t *testing.T) {
	narrowPolicy(t)
	st := &failingResultNarrowStore{narrowStore: &narrowStore{approval: narrowApprovalRow()}}
	s := narrowServer(t, st, &narrowChild{})
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["a"]}`)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body["execution_unknown"] != true || body["narrowed"] == nil {
		t.Fatalf("answer = %d %s, want execution_unknown with the narrowing", rec.Code, rec.Body.String())
	}
}

// An outbound email send is never narrowable: a key that selects one is
// dropped at boot, and the run-time predicate refuses one too (defence in
// depth, should a later policy path let such a key through).
func TestStagedNarrowable_NeverAnEmailSend(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"mail_send_email"},
		CriticalToolNarrowableArgs: map[string]string{"mail_send_email": "to"},
	})
	if _, _, ok := stagedNarrowable("mcp_x_mail_send_email", `{"to":["a@x.com","b@x.com"]}`); ok {
		t.Fatal("an email send must never be narrowable")
	}
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"x_send_template_email"},
		CriticalToolNarrowableArgs: map[string]string{"x_send_template_email": "to"},
	})
	if _, _, ok := stagedNarrowable("mcp_x_send_template_email", `{"to":["a@x.com","b@x.com"]}`); ok {
		t.Fatal("a template email send must never be narrowable")
	}
	for _, tool := range []string{"mcp_x_send_email", "mcp_x_send_template_email", "send_email"} {
		if !agentcore.IsOutboundEmailTool(tool) {
			t.Errorf("%s: not recognized as an outbound email send", tool)
		}
	}
	if agentcore.IsOutboundEmailTool("mcp_ix_execute_plan") {
		t.Error("execute_plan is not an email send")
	}
}

// A grouped decision gets its own body cap, not the global 1 MiB JSON cap;
// every other POST keeps the global one.
func TestBodyLimitMiddleware_ApprovalGroupHasItsOwnCap(t *testing.T) {
	big := strings.Repeat("x", maxJSONBodyBytes+10)
	read := func(path string) error {
		var err error
		h := bodyLimitMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, err = io.ReadAll(r.Body)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(big)))
		return err
	}
	if err := read("/conversations/c1/approval-groups/g1"); err != nil {
		t.Fatalf("the group decision was cut by the global cap: %v", err)
	}
	for _, path := range []string{"/conversations/c1/approvals/a1", "/conversations/c1/approval-groups/g1/x", "/conversations/c1/messages"} {
		if err := read(path); err == nil {
			t.Errorf("%s: the global cap must still apply", path)
		}
	}
}

// The group cap holds the largest decision the endpoint accepts, written in
// the most expensive JSON escaping (a surrogate pair per character).
func TestMaxApprovalGroupBody_HoldsTheWorstCaseEscaping(t *testing.T) {
	perValue := maxNarrowableValueRunes*12 + 3 // two six-byte escapes per rune, quotes, comma
	ids := maxApprovalGroupDecisions * 200     // ids, keys and brackets, generously
	if need := maxApprovalGroupNarrowedValues*perValue + ids; need > maxApprovalGroupBody {
		t.Fatalf("worst case %d bytes > cap %d", need, maxApprovalGroupBody)
	}
}
