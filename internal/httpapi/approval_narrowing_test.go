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
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover approval narrowing (docs/APPROVAL-NARROWING.md,
// ADR-0089): a person approves only some of the staged values of a tool's
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
		NarrowableArg: "deal_ids", // frozen at staging
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

	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["c","a"],"narrowed_arg":"deal_ids"}`)
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
	if !strings.HasPrefix(st.result, `{"success":true}`) || !strings.Contains(st.result, `of the 3 staged deal_ids in its arguments, only positions 2 were left out; the other 2 were approved. The call's deal_ids held only the approved ones`) {
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
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["b","c","a"],"narrowed_arg":"deal_ids"}`)
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
		{name: "empty", body: `{"approved":true,"approved_values":[],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "explicit null", body: `{"approved":true,"approved_values":null,"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "unknown value", body: `{"approved":true,"approved_values":["a","z"],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "repeated value", body: `{"approved":true,"approved_values":["a","a"],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "with a decline", body: `{"approved":false,"approved_values":["a"],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "with a session scope", body: `{"approved":true,"scope":"session","approved_values":["a"],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "tool declares nothing", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"deal_ids"}`},
		{name: "one staged value", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"deal_ids"}`, args: `{"deal_ids":["a"]}`, policy: true},
		{name: "staged list repeats a value", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"deal_ids"}`, args: `{"deal_ids":["a","a","b"]}`, policy: true},
		{name: "staged list holds a number", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"deal_ids"}`, args: `{"deal_ids":["a",7]}`, policy: true},
		{name: "no narrowed_arg", body: `{"approved":true,"approved_values":["a"]}`, policy: true},
		{name: "narrowed_arg without approved_values", body: `{"approved":true,"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "narrowed_arg with a decline", body: `{"approved":false,"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "narrowed_arg for a tool that declares nothing", body: `{"approved":true,"narrowed_arg":"deal_ids"}`},
		{name: "more values than were staged", body: `{"approved":true,"approved_values":["a","b","c","a"],"narrowed_arg":"deal_ids"}`, policy: true},
		{name: "a narrowed_arg other than the one frozen at staging", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"item_ids"}`, policy: true},
		{name: "staged value too long for a row key", body: `{"approved":true,"approved_values":["a"],"narrowed_arg":"deal_ids"}`, args: `{"deal_ids":["a","` + strings.Repeat("x", 201) + `"]}`, policy: true},
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
			if !tc.policy {
				row.NarrowableArg = "" // nothing was frozen at staging
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
	pending := narrowApprovalRow()
	fields := withApprovalNarrowable(map[string]any{}, &pending)
	want := map[string]any{"arg": "deal_ids", "values": []string{"a", "b", "c"}}
	if !reflect.DeepEqual(fields["narrowable"], want) {
		t.Fatalf("narrowable = %#v, want %#v", fields["narrowable"], want)
	}
	for _, tc := range []struct{ tool, args, frozen string }{
		{"mcp_pages_deploy_page", `{"deal_ids":["a","b"]}`, ""},
		{"mcp_ix_execute_plan", `{"deal_ids":["a"]}`, "deal_ids"},
		{"mcp_ix_execute_plan", `{"deal_ids":"a,b"}`, "deal_ids"},
		{"mcp_ix_execute_plan", `{}`, "deal_ids"},
		{"mcp_ix_execute_plan", narrowStagedArgs, ""}, // nothing frozen at staging
	} {
		row := store.Approval{ToolName: tc.tool, ArgsJSON: tc.args, Status: "pending", NarrowableArg: tc.frozen}
		if _, ok := withApprovalNarrowable(map[string]any{}, &row)["narrowable"]; ok {
			t.Errorf("%s %s (frozen %q): narrowable present, want absent", tc.tool, tc.args, tc.frozen)
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
			r[i].NarrowableArg = "deal_ids" // frozen at staging
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
	rec := post(st, `{"approve":["ap-pm","ap-mg"],"decline":["ap-ix"],"narrow":{"ap-pm":["z","x"]},"narrow_args":{"ap-pm":"deal_ids"}}`)
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
		`{"approve":["ap-pm"],"decline":["ap-mg"],"narrow":{"ap-mg":["x"]}}`,
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":["x"]}}`,                                                                               // no narrow_args
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":["x"]},"narrow_args":{"ap-pm":"item_ids"}}`,                                            // an argument the policy no longer narrows
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":["x"]},"narrow_args":{"ap-pm":"deal_ids","ap-mg":"deal_ids"}}`,                         // narrow_args for a card not narrowed                                                                   // narrowing a declined card
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":["q"]},"narrow_args":{"ap-pm":"deal_ids"}}`,                                            // not a staged value
		`{"approve":["ap-pm","ap-mg"],"narrow":{"ap-pm":[]},"narrow_args":{"ap-pm":"deal_ids"}}`,                                               // empty
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

// Near the minimum output limit the exact narrowing line of a long,
// scattered selection (about 1.6 KiB) cannot fit beside the tool's text. The
// line is never shortened, so that selection is refused at decision with
// nothing claimed; a selection whose exact line fits is accepted and the
// whole result stays within the configured limit, the line whole at its end.
func TestNarrowing_StaysExactAndWithinAMinimumOutputLimit(t *testing.T) {
	narrowPolicy(t)
	agentcore.SetMaxToolOutputBytes(agentcore.MinMaxToolOutputBytes)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	staged := make([]string, 0, 500)
	for i := range 500 {
		staged = append(staged, fmt.Sprintf("d%03d", i))
	}
	stagedJSON, _ := json.Marshal(map[string]any{"deal_ids": staged})
	row := narrowApprovalRow()
	row.ArgsJSON = string(stagedJSON)

	var scattered []string
	for i, v := range staged {
		if i%2 == 0 {
			scattered = append(scattered, v)
		}
	}
	if _, err := narrowApproval(&row, scattered, "deal_ids"); err == nil || !strings.Contains(err.Error(), "too scattered") {
		t.Fatalf("a scattered 250-of-500 selection: err = %v, want refused", err)
	}

	n, err := narrowApproval(&row, staged[:250], "deal_ids") // positions 1-250: a short exact line
	if err != nil || n == nil {
		t.Fatalf("a contiguous selection: %v", err)
	}
	trailer := approvalTrailer(n)
	if trailer != narrowingNote(n) || !strings.Contains(trailer, "positions 1-250") {
		t.Fatalf("trailer = %q, want the exact line", trailer)
	}
	for _, text := range []string{strings.Repeat("x", 64<<10), `{"success":true}`} {
		got, _, _ := governApprovalResultWithTrailer(context.Background(), &row, text, false, trailer)
		if !strings.HasSuffix(got, trailer) {
			t.Fatalf("the line was cut or changed: ...%q", got[max(0, len(got)-200):])
		}
		if len(got) > agentcore.MinMaxToolOutputBytes {
			t.Fatalf("result is %d bytes, over the configured %d", len(got), agentcore.MinMaxToolOutputBytes)
		}
	}
	// A roomier limit live now does not change that: the limit may be
	// lowered to the minimum before the call settles, so the line must fit
	// the room there.
	agentcore.SetMaxToolOutputBytes(64 << 10)
	if _, err := narrowApproval(&row, scattered, "deal_ids"); err == nil || !strings.Contains(err.Error(), "too scattered") {
		t.Fatalf("at 64 KiB the scattered selection: err = %v, want refused all the same", err)
	}
	if approvalTrailer(nil, "a", "b") != "a\n\nb" || approvalTrailer(nil) != "" {
		t.Fatal("other lines join as given")
	}
}

// A narrowing accepted under a roomy limit still settles within the limit
// live when the call ends: an operator who lowers max_tool_output_bytes to
// the minimum while the call runs gets a result within it, the line whole.
func TestNarrowing_SettlesWithinALimitLoweredWhileItRan(t *testing.T) {
	narrowPolicy(t)
	agentcore.SetMaxToolOutputBytes(64 << 10)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	staged := make([]string, 0, 500)
	for i := range 500 {
		staged = append(staged, fmt.Sprintf("d%03d", i))
	}
	stagedJSON, _ := json.Marshal(map[string]any{"deal_ids": staged})
	row := narrowApprovalRow()
	row.ArgsJSON = string(stagedJSON)
	// The longest line accepted: leave out one more scattered position at a
	// time (every other one from 300 on) until the selection is refused.
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
	trailer := approvalTrailer(n)
	if len(trailer) < agentcore.ApprovalTrailerRoom()-8 {
		t.Fatalf("test setup: the line is %d bytes, want close to the room %d", len(trailer), agentcore.ApprovalTrailerRoom())
	}
	agentcore.SetMaxToolOutputBytes(agentcore.MinMaxToolOutputBytes) // lowered while the call ran
	got, _, _ := governApprovalResultWithTrailer(context.Background(), &row, strings.Repeat("x", 64<<10), false, trailer)
	if !strings.HasSuffix(got, trailer) {
		t.Fatalf("the line was cut or changed: ...%q", got[max(0, len(got)-200):])
	}
	if len(got) > agentcore.MinMaxToolOutputBytes {
		t.Fatalf("result is %d bytes, over the %d-byte limit live at settlement", len(got), agentcore.MinMaxToolOutputBytes)
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
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["a","c"],"narrowed_arg":"deal_ids"}`)
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
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once","approved_values":["a"],"narrowed_arg":"deal_ids"}`)
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
	if _, _, ok := stagedNarrowable("mcp_x_mail_send_email", `{"to":["a@x.com","b@x.com"]}`, "to"); ok || narrowableArgAtStaging("mcp_x_mail_send_email", `{"to":["a@x.com","b@x.com"]}`) != "" {
		t.Fatal("an email send must never be narrowable")
	}
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"x_send_template_email"},
		CriticalToolNarrowableArgs: map[string]string{"x_send_template_email": "to"},
	})
	if _, _, ok := stagedNarrowable("mcp_x_send_template_email", `{"to":["a@x.com","b@x.com"]}`, "to"); ok || narrowableArgAtStaging("mcp_x_send_template_email", `{"to":["a@x.com","b@x.com"]}`) != "" {
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

// A tool of a synthetic server (inline http_tools, a2a peers) is never
// narrowable, whatever key matches it: its transport forwards the arguments
// only, so the endpoint would never receive the staged list in
// `_meta["fleet/narrowed"]`.
func TestStagedNarrowable_NeverASyntheticTransportTool(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "deal_ids"},
	})
	args := `{"deal_ids":["d1","d2","d3"]}`
	for _, tool := range []string{
		"mcp_" + mcp.HTTPToolServerName + "_execute_plan",
		"mcp_" + mcp.A2AToolServerName + "_execute_plan",
	} {
		if _, _, ok := stagedNarrowable(tool, args, "deal_ids"); ok || narrowableArgAtStaging(tool, args) != "" {
			t.Errorf("%s: a synthetic-transport tool must never be narrowable", tool)
		}
		if _, err := narrowApproval(&store.Approval{ToolName: tool, ArgsJSON: args, NarrowableArg: "deal_ids"}, []string{"d1"}, "deal_ids"); err == nil {
			t.Errorf("%s: a narrowing must be refused", tool)
		}
	}
	// The same key still narrows a real MCP server's tool.
	if arg := narrowableArgAtStaging("mcp_dealplan_execute_plan", args); arg != "deal_ids" {
		t.Fatalf("an MCP tool must stay narrowable at staging: arg=%q", arg)
	}
	if arg, values, ok := stagedNarrowable("mcp_dealplan_execute_plan", args, "deal_ids"); !ok || arg != "deal_ids" || len(values) != 3 {
		t.Fatalf("an MCP tool must stay narrowable: arg=%q values=%v ok=%v", arg, values, ok)
	}
}

// The argument is frozen on the card at staging: a restart with another
// critical_tool_narrowable_args entry changes neither what a reload of the
// pending card offers nor what its decision narrows, and a card staged with
// nothing frozen is never narrowed whatever the policy says now.
func TestNarrowing_UsesTheArgumentFrozenAtStaging(t *testing.T) {
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:       []string{"execute_plan"},
		CriticalToolNarrowableArgs: map[string]string{"execute_plan": "item_ids"},
	})
	row := narrowApprovalRow()
	row.ArgsJSON = `{"deal_ids":["a","b","c"],"item_ids":["a","b","c"]}`
	want := map[string]any{"arg": "deal_ids", "values": []string{"a", "b", "c"}}
	if got := withApprovalNarrowable(map[string]any{}, &row)["narrowable"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded narrowable = %#v, want the frozen %#v", got, want)
	}
	n, err := narrowApproval(&row, []string{"a"}, "deal_ids")
	if err != nil || n == nil || n.Arg != "deal_ids" {
		t.Fatalf("narrowApproval = %+v, %v; want the frozen deal_ids", n, err)
	}
	if _, err := narrowApproval(&row, []string{"a"}, "item_ids"); err == nil {
		t.Fatal("a decision naming the policy's new argument must be refused")
	}
	row.NarrowableArg = ""
	if _, ok := withApprovalNarrowable(map[string]any{}, &row)["narrowable"]; ok {
		t.Fatal("a card staged with nothing frozen must not become narrowable")
	}
	if _, err := narrowApproval(&row, []string{"a"}, "item_ids"); err == nil {
		t.Fatal("a card staged with nothing frozen must refuse a narrowing")
	}
}

// narrowStageStore records the argument the stager freezes.
type narrowStageStore struct {
	stageStore
	frozen string
}

func (s *narrowStageStore) SetApprovalNarrowableArg(_ context.Context, _, _, arg string) (bool, error) {
	s.frozen = arg
	return true, nil
}

// Staging freezes the policy's argument on the row and the live card carries
// it; a call whose staged value is not a narrowable list freezes nothing.
func TestStage_FreezesTheNarrowableArgument(t *testing.T) {
	narrowPolicy(t)
	st := &narrowStageStore{}
	sink := &eventSink{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: sink}
	if _, err := a.Stage("mcp_ix_execute_plan", "call-1", narrowStagedArgs); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if st.frozen != "deal_ids" || st.created.NarrowableArg != "deal_ids" {
		t.Fatalf("frozen = %q / row %q, want deal_ids", st.frozen, st.created.NarrowableArg)
	}
	ev := eventNamed(sink, "tool.approval_required")
	if want := (map[string]any{"arg": "deal_ids", "values": []string{"a", "b", "c"}}); !reflect.DeepEqual(ev["narrowable"], want) {
		t.Fatalf("event narrowable = %#v, want %#v", ev["narrowable"], want)
	}
	st2 := &narrowStageStore{}
	a2 := &approvalStager{ctx: context.Background(), store: st2, conversationID: "c1", userEmail: "u@e.com", sink: &eventSink{}}
	if _, err := a2.Stage("mcp_ix_execute_plan", "call-2", `{"deal_ids":["a"]}`); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if st2.frozen != "" {
		t.Fatalf("frozen = %q, want nothing for a one-value list", st2.frozen)
	}
}

// A request naming more values than were staged is refused by its length,
// before anything is sized by it (a body can hold hundreds of thousands of
// short repeats).
func TestNarrowApproval_RefusesMoreValuesThanStagedByLength(t *testing.T) {
	narrowPolicy(t)
	row := narrowApprovalRow()
	huge := make([]string, 200000)
	for i := range huge {
		huge[i] = "a"
	}
	_, err := narrowApproval(&row, huge, "deal_ids")
	if err == nil || !strings.Contains(err.Error(), "more than the 3 staged") {
		t.Fatalf("err = %v, want the length refusal", err)
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
	for _, path := range []string{"/conversations/c1/approval-groups/g1", "/conversations/c1/approvals/a1"} {
		if err := read(path); err != nil {
			t.Fatalf("%s: an approval decision was cut by the global cap: %v", path, err)
		}
	}
	for _, path := range []string{"/conversations/c1/approvals/a1/stop", "/conversations/c1/approval-groups/g1/x", "/conversations/c1/messages"} {
		if err := read(path); err == nil {
			t.Errorf("%s: the global cap must still apply", path)
		}
	}
}

// The group cap holds the largest decision the endpoint accepts, written in
// the most expensive JSON escaping (a surrogate pair per character).
func TestMaxApprovalGroupBody_HoldsTheWorstCaseEscaping(t *testing.T) {
	perValue := maxNarrowableValueRunes*12 + 3        // two six-byte escapes per rune, quotes, comma
	ids := maxApprovalGroupDecisions * (200 + 64 + 8) // ids, keys and brackets, generously; a narrow_args name (64) each
	if need := maxApprovalGroupNarrowedValues*perValue + ids; need > maxApprovalGroupBody {
		t.Fatalf("worst case %d bytes > cap %d", need, maxApprovalGroupBody)
	}
}

// The single-card cap holds the largest narrowing it accepts, in the most
// expensive JSON escaping.
func TestMaxApprovalBody_HoldsTheWorstCaseEscaping(t *testing.T) {
	if need := maxNarrowableValues*(maxNarrowableValueRunes*12+3) + 1024; need > maxApprovalBody {
		t.Fatalf("worst case %d bytes > cap %d", need, maxApprovalBody)
	}
}

// An approved value longer than any staged value can be is refused by its
// position, never quoted back: one multi-megabyte value gets a short error.
// A value that is merely unknown is quoted, cut to a bounded length; so is a
// huge echoed argument name.
func TestNarrowApproval_ErrorsNeverEchoAHugeValue(t *testing.T) {
	narrowPolicy(t)
	row := narrowApprovalRow()
	huge := strings.Repeat("é", 2<<20)
	_, err := narrowApproval(&row, []string{"a", huge}, "deal_ids")
	if err == nil || !strings.Contains(err.Error(), "item 2 is longer than") || len(err.Error()) > 300 {
		t.Fatalf("err = %.300v (%d bytes), want the bounded length refusal", err, len(fmt.Sprint(err)))
	}
	long := strings.Repeat("z", maxNarrowableValueRunes)
	_, err = narrowApproval(&row, []string{"a", long}, "deal_ids")
	if err == nil || !strings.Contains(err.Error(), "not one of the staged") || strings.Contains(err.Error(), long) || len(err.Error()) > 300 {
		t.Fatalf("err = %v, want an unknown-value refusal quoting a cut value", err)
	}
	_, err = narrowApproval(&row, []string{"a"}, huge)
	if err == nil || !strings.Contains(err.Error(), "now narrows") || len(err.Error()) > 400 {
		t.Fatalf("err = %.300v (%d bytes), want a bounded changed-argument refusal", err, len(fmt.Sprint(err)))
	}
}

func TestQuoteClientValue(t *testing.T) {
	if got := quoteClientValue("ap-1"); got != `"ap-1"` {
		t.Fatalf("short = %s", got)
	}
	exact := strings.Repeat("é", maxQuotedClientRunes)
	if got := quoteClientValue(exact); got != fmt.Sprintf("%q", exact) {
		t.Fatalf("exactly at the bound = %s, want it whole", got)
	}
	got := quoteClientValue(exact + "\nmore")
	if got != fmt.Sprintf("%q…", exact) {
		t.Fatalf("over the bound = %s, want the first %d characters and an ellipsis", got, maxQuotedClientRunes)
	}
}

// The restart breadcrumb of the longest narrowing fleet accepts (the outcome
// line plus the exact narrowing line) stays within the minimum output limit,
// so a restart can never write a recovered result past any limit the
// deployment can be set to; the line is whole in it.
func TestApprovalRecoveryText_FitsTheMinimumOutputLimit(t *testing.T) {
	narrowPolicy(t)
	agentcore.SetMaxToolOutputBytes(64 << 10)
	t.Cleanup(func() { agentcore.SetMaxToolOutputBytes(-1) })
	staged := make([]string, 0, 500)
	for i := range 500 {
		staged = append(staged, fmt.Sprintf("d%03d", i))
	}
	stagedJSON, _ := json.Marshal(map[string]any{"deal_ids": staged})
	row := narrowApprovalRow()
	row.ArgsJSON = string(stagedJSON)
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
	record, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	text := store.ApprovalRecoveryText(string(record))
	if !strings.HasSuffix(text, narrowingNote(n)) || !strings.HasPrefix(text, store.ApprovalRecoveryOutcome) {
		t.Fatalf("breadcrumb = %q, want the outcome then the whole narrowing line", text)
	}
	if len(text) > agentcore.MinMaxToolOutputBytes {
		t.Fatalf("breadcrumb is %d bytes, over the %d-byte minimum limit", len(text), agentcore.MinMaxToolOutputBytes)
	}
}

// The largest list a card may narrow, in its worst supported encoding (500
// values of 200 characters, outside the BMP and written as surrogate-pair
// escapes), is over 1 MiB of arguments: it is still narrowable, and its
// review snapshot is complete (frozen_args is bounded re-encoded, about
// 400 KiB here), so the terminal can approve it too. Only arguments past
// approvalArgsMaxDecodeBytes approve whole.
func TestStagedNarrowable_HoldsTheWorstCaseEscapedList(t *testing.T) {
	narrowPolicy(t)
	var b strings.Builder
	b.WriteString(`{"deal_ids":[`)
	// One character outside the BMP as a JSON surrogate-pair escape (12 bytes).
	escaped := `\` + "ud83d" + `\` + "ude00"
	emoji := strings.Repeat(escaped, maxNarrowableValueRunes-4)
	for i := range maxNarrowableValues {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%s%04d"`, emoji, i)
	}
	b.WriteString(`]}`)
	raw := b.String()
	if len(raw) <= frozenArgsMaxBytes {
		t.Fatalf("test setup: %d bytes, want past the 1 MiB frozen-args bound", len(raw))
	}
	arg, values, ok := stagedNarrowable("mcp_ix_execute_plan", raw, "deal_ids")
	if !ok || arg != "deal_ids" || len(values) != maxNarrowableValues || utf8.RuneCountInString(values[0]) != maxNarrowableValueRunes {
		t.Fatalf("worst-case list (%d bytes): ok=%t arg=%q values=%d", len(raw), ok, arg, len(values))
	}
	if frozen := frozenApprovalArgs(raw); frozen["complete"] != true {
		t.Fatalf("the worst-case list's review snapshot is incomplete, so the terminal could not approve it: %v", frozen["complete"])
	}
	padded := `{"pad":"` + strings.Repeat("x", approvalArgsMaxDecodeBytes) + `","deal_ids":["a","b"]}`
	if _, _, ok := stagedNarrowable("mcp_ix_execute_plan", padded, "deal_ids"); ok {
		t.Fatal("arguments past approvalArgsMaxDecodeBytes must approve whole")
	}
	if frozenApprovalArgs(padded)["complete"] != false {
		t.Fatal("arguments past approvalArgsMaxDecodeBytes are not reviewable in full")
	}
}
