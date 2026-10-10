package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// groupPolicy opts execute_plan into grouped approvals for one test.
func groupPolicy(t *testing.T) {
	t.Helper()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:      []string{"execute_plan", "deploy_page"},
		CriticalToolGroupApproval: []string{"execute_plan"},
	})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
}

// groupStore is an approvals store with the real claim discipline for
// several rows: one pending→resolved winner per row, the executing sentinel
// until the outcome is written, and counters of claims, outcomes and
// history breadcrumbs per row.
type groupStore struct {
	*store.Store
	mu        sync.Mutex
	approvals map[string]*store.Approval
	claims    map[string]int
	results   map[string]int
	history   map[string][]string // conversation → breadcrumb texts
	grouped   map[string]string   // SetApprovalGroup writes
}

func newGroupStore(rows ...store.Approval) *groupStore {
	g := &groupStore{
		approvals: map[string]*store.Approval{},
		claims:    map[string]int{},
		results:   map[string]int{},
		history:   map[string][]string{},
		grouped:   map[string]string{},
	}
	for i := range rows {
		a := rows[i]
		if a.Status == "" {
			a.Status = "pending"
		}
		if a.UserEmail == "" {
			a.UserEmail = "u@example.com"
		}
		g.approvals[a.ID] = &a
	}
	return g
}

func (g *groupStore) GetApproval(_ context.Context, user, id string) (*store.Approval, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.approvals[id]
	if !ok || a.UserEmail != user {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

func (g *groupStore) ClaimApproval(_ context.Context, user, id, status, text string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.approvals[id]
	if !ok || a.UserEmail != user || a.Status != "pending" {
		return false, nil
	}
	g.claims[id]++
	a.Status, a.ResultText = status, text
	return true, nil
}

func (g *groupStore) SetApprovalResult(_ context.Context, _, id, text string, isErr bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.results[id]++
	a := g.approvals[id]
	a.ResultText = text
	a.IsErr = sql.NullBool{Valid: true, Bool: isErr}
	return nil
}

func (g *groupStore) AppendHistory(_ context.Context, convID string, entries []agent.HistoryEntry) ([]int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range entries {
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		g.history[convID] = append(g.history[convID], c.Text)
	}
	return nil, nil
}

func (g *groupStore) SetApprovalGroup(_ context.Context, _, id, groupID string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grouped[id] = groupID
	return true, nil
}

func (g *groupStore) state(id string) store.Approval {
	g.mu.Lock()
	defer g.mu.Unlock()
	return *g.approvals[id]
}

func (g *groupStore) claimCount(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.claims[id]
}

func groupRows() []store.Approval {
	return []store.Approval{
		{ID: "ap-pm", ConversationID: "c1", ToolName: "mcp_pubmatic_execute_plan", ToolCallID: "tc-pm", GroupID: "turn-1"},
		{ID: "ap-mg", ConversationID: "c1", ToolName: "mcp_magnite_execute_plan", ToolCallID: "tc-mg", GroupID: "turn-1"},
		{ID: "ap-ix", ConversationID: "c1", ToolName: "mcp_ix_execute_plan", ToolCallID: "tc-ix", GroupID: "turn-1"},
	}
}

// groupMockServer runs approved calls in mock mode: a canned success, no MCP.
func groupMockServer(st chatStore) *Server {
	return &Server{store: st, cfg: &config.Config{MockMode: true}}
}

type groupReply struct {
	GroupID string                `json:"group_id"`
	Results []approvalGroupResult `json:"results"`
	Resume  bool                  `json:"resume"`
}

// postGroup posts a group decision for conversation c1.
func postGroup(t *testing.T, s *Server, method, group, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, "/conversations/c1/approval-groups/"+url.PathEscape(group), strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.handleApprovalGroup(rec, req, "c1", group)
	return rec.Code, rec.Body.String()
}

func decodeGroupReply(t *testing.T, body string) map[string]approvalGroupResult {
	t.Helper()
	var reply groupReply
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("bad reply %q: %v", body, err)
	}
	out := map[string]approvalGroupResult{}
	for _, r := range reply.Results {
		out[r.ApprovalID] = r
	}
	return out
}

// Approve all approves the checked cards and declines the unchecked one, each
// through its own claim and outcome write, exactly as a single-card POST would.
func TestApprovalGroup_ApprovesCheckedDeclinesUnchecked(t *testing.T) {
	groupPolicy(t)
	st := newGroupStore(groupRows()...)
	s := groupMockServer(st)

	code, body := postGroup(t, s, http.MethodPost, "turn-1", `{"approve":["ap-pm","ap-mg"],"decline":["ap-ix"]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	got := decodeGroupReply(t, body)
	for _, id := range []string{"ap-pm", "ap-mg"} {
		r := got[id]
		if r.Decision != "approve" || r.StatusCode != http.StatusOK || r.Result["status"] != "approved" || r.Result["is_err"] != false {
			t.Errorf("%s = %+v, want approved with a recorded success", id, r)
		}
		if a := st.state(id); a.Status != "approved" || !a.IsErr.Valid || a.IsErr.Bool {
			t.Errorf("%s row = %+v, want approved with its outcome written", id, a)
		}
		if st.claimCount(id) != 1 || st.results[id] != 1 {
			t.Errorf("%s claims=%d results=%d, want exactly one of each", id, st.claimCount(id), st.results[id])
		}
	}
	if r := got["ap-ix"]; r.Decision != "decline" || r.Result["status"] != "rejected" {
		t.Errorf("ap-ix = %+v, want declined", r)
	}
	if a := st.state("ap-ix"); a.Status != "rejected" {
		t.Errorf("unchecked card row = %+v, want rejected", a)
	}
	if h := strings.Join(st.history["c1"], "\n"); !strings.Contains(h, "declined") {
		t.Errorf("history = %q, want the decline breadcrumb for the unchecked card", h)
	}
}

// A replayed group decision (a lost answer, a second tab) claims nothing
// again: every card answers its recorded state, and a card the person settled
// individually in between keeps that decision.
func TestApprovalGroup_ReplayIsIdempotent(t *testing.T) {
	groupPolicy(t)
	st := newGroupStore(groupRows()...)
	s := groupMockServer(st)

	if code, body := postGroup(t, s, http.MethodPost, "turn-1", `{"approve":["ap-pm"],"decline":["ap-mg"]}`); code != http.StatusOK {
		t.Fatalf("first POST = %d %s", code, body)
	}
	// The same body again, plus the card the person declined meanwhile.
	code, body := postGroup(t, s, http.MethodPost, "turn-1", `{"approve":["ap-pm","ap-mg"]}`)
	if code != http.StatusOK {
		t.Fatalf("replay = %d %s", code, body)
	}
	got := decodeGroupReply(t, body)
	if got["ap-pm"].Result["status"] != "approved" || got["ap-mg"].Result["status"] != "rejected" {
		t.Fatalf("replay answers = %+v, want each card's recorded outcome", got)
	}
	if st.claimCount("ap-pm") != 1 || st.claimCount("ap-mg") != 1 {
		t.Fatalf("claims pm=%d mg=%d, want one each: a replay never re-runs", st.claimCount("ap-pm"), st.claimCount("ap-mg"))
	}
	if a := st.state("ap-ix"); a.Status != "pending" {
		t.Fatalf("a card named in neither list = %+v, want left pending", a)
	}
}

// A decision naming any card outside this conversation or group, an unknown
// card, a duplicate, or nothing at all is refused whole: nothing is claimed.
func TestApprovalGroup_RefusesWholeDecisionBeforeClaiming(t *testing.T) {
	groupPolicy(t)
	rows := append(groupRows(),
		store.Approval{ID: "ap-other-group", ConversationID: "c1", ToolName: "mcp_ix_execute_plan", GroupID: "turn-2"},
		store.Approval{ID: "ap-ungrouped", ConversationID: "c1", ToolName: "mcp_pages_deploy_page"},
		store.Approval{ID: "ap-other-conv", ConversationID: "c2", ToolName: "mcp_ix_execute_plan", GroupID: "turn-1"},
		store.Approval{ID: "ap-foreign", ConversationID: "c1", ToolName: "mcp_ix_execute_plan", GroupID: "turn-1", UserEmail: "mallory@example.com"},
	)
	for name, tc := range map[string]struct {
		method, body string
		want         int
		group        string // "" = turn-1
	}{
		"group in the path is not the cards' group": {http.MethodPost, `{"approve":["ap-pm","ap-mg"]}`, http.StatusConflict, "turn-2"},
		"blank group id":      {http.MethodPost, `{"approve":["ap-pm"]}`, http.StatusBadRequest, " "},
		"other group":         {http.MethodPost, `{"approve":["ap-pm"],"decline":["ap-other-group"]}`, http.StatusConflict, ""},
		"ungrouped card":      {http.MethodPost, `{"approve":["ap-pm","ap-ungrouped"]}`, http.StatusConflict, ""},
		"other conversation":  {http.MethodPost, `{"approve":["ap-pm","ap-other-conv"]}`, http.StatusNotFound, ""},
		"another user's card": {http.MethodPost, `{"approve":["ap-pm","ap-foreign"]}`, http.StatusNotFound, ""},
		"unknown card":        {http.MethodPost, `{"approve":["ap-pm","nope"]}`, http.StatusNotFound, ""},
		"duplicate":           {http.MethodPost, `{"approve":["ap-pm"],"decline":["ap-pm"]}`, http.StatusBadRequest, ""},
		"empty id":            {http.MethodPost, `{"approve":["ap-pm",""]}`, http.StatusBadRequest, ""},
		"nothing named":       {http.MethodPost, `{"approve":[],"decline":[]}`, http.StatusBadRequest, ""},
		"unknown field":       {http.MethodPost, `{"approve":["ap-pm"],"scope":"session"}`, http.StatusBadRequest, ""},
		"trailing data":       {http.MethodPost, `{"approve":["ap-pm"]} {}`, http.StatusBadRequest, ""},
		"wrong method":        {http.MethodGet, ``, http.StatusMethodNotAllowed, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := newGroupStore(rows...)
			group := tc.group
			if group == "" {
				group = "turn-1"
			}
			code, body := postGroup(t, groupMockServer(st), tc.method, group, tc.body)
			if code != tc.want {
				t.Fatalf("status = %d (%s), want %d", code, body, tc.want)
			}
			for id := range st.approvals {
				if n := st.claimCount(id); n != 0 {
					t.Fatalf("%s claimed %d times by a refused decision", id, n)
				}
			}
		})
	}
}

// Each card keeps its own outcome: while a shutdown drain refuses new
// executions, the approved cards answer 503 and stay pending (they can be
// approved after the restart), while the declined card still settles.
func TestApprovalGroup_PerCardOutcomeDuringDrain(t *testing.T) {
	groupPolicy(t)
	st := newGroupStore(groupRows()...)
	s := groupMockServer(st)
	if !s.approvalRuns.drain(context.Background()) {
		t.Fatal("drain with nothing running must finish")
	}

	code, body := postGroup(t, s, http.MethodPost, "turn-1", `{"approve":["ap-pm","ap-mg"],"decline":["ap-ix"]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	got := decodeGroupReply(t, body)
	for _, id := range []string{"ap-pm", "ap-mg"} {
		if r := got[id]; r.StatusCode != http.StatusServiceUnavailable || r.Error == "" || r.Result != nil {
			t.Errorf("%s = %+v, want a 503 with its reason", id, r)
		}
		if a := st.state(id); a.Status != "pending" || st.claimCount(id) != 0 {
			t.Errorf("%s = %+v claims=%d, want still pending and unclaimed", id, a, st.claimCount(id))
		}
	}
	if r := got["ap-ix"]; r.StatusCode != http.StatusOK || r.Result["status"] != "rejected" {
		t.Errorf("ap-ix = %+v, want the decline recorded", r)
	}
}

// Staging an opted-in tool records the turn's group on the card and puts it
// on the live event; a tool that did not opt in, or a stager with no turn
// group, records nothing and its event carries no group_id key.
func TestStage_JoinsTheTurnsApprovalGroup(t *testing.T) {
	groupPolicy(t)
	st := &groupStageStore{groupStore: newGroupStore()}
	sink := &eventSink{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@example.com", sink: sink, groupID: "turn-1"}

	if _, err := a.Stage("mcp_pubmatic_execute_plan", "call-1", `{"plan":"p1"}`); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if st.grouped["ap-1"] != "turn-1" {
		t.Fatalf("SetApprovalGroup = %v, want ap-1 in turn-1", st.grouped)
	}
	ev := lastApprovalRequired(t, sink)
	if ev["group_id"] != "turn-1" {
		t.Fatalf("approval_required group_id = %v, want turn-1", ev["group_id"])
	}

	if _, err := a.Stage("mcp_pages_deploy_page", "call-2", `{"slug":"q3"}`); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, ok := st.grouped["ap-2"]; ok {
		t.Fatal("a tool that did not opt in must not join the group")
	}
	if _, ok := lastApprovalRequired(t, sink)["group_id"]; ok {
		t.Fatal("an ungrouped card's event must carry no group_id key")
	}

	ungrouped := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@example.com", sink: sink}
	if _, err := ungrouped.Stage("mcp_ix_execute_plan", "call-3", `{}`); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, ok := st.grouped["ap-3"]; ok {
		t.Fatal("a stager with no turn group must group nothing")
	}
}

// groupStageStore adds what Stage needs on top of groupStore.
type groupStageStore struct {
	*groupStore
	n int
}

func (g *groupStageStore) SupersedePendingApprovals(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (g *groupStageStore) CreateApproval(_ context.Context, conv, user, tool, call, args string, expiry int64, _ store.ApprovalSeat) (*store.Approval, error) {
	g.n++
	a := &store.Approval{ID: "ap-" + string(rune('0'+g.n)), ConversationID: conv, UserEmail: user, ToolName: tool, ToolCallID: call, ArgsJSON: args, ExpiresAt: expiry, Status: "pending"}
	return a, nil
}

func lastApprovalRequired(t *testing.T, sink *eventSink) map[string]any {
	t.Helper()
	for i := len(sink.names) - 1; i >= 0; i-- {
		if sink.names[i] == "tool.approval_required" {
			ev, _ := sink.payloads[i].(map[string]any)
			return ev
		}
	}
	t.Fatal("no tool.approval_required event")
	return nil
}

// The group id rides every client payload of a grouped card and no other.
func TestWithApprovalGroup(t *testing.T) {
	if got := withApprovalGroup(map[string]any{}, &store.Approval{GroupID: "turn-1"}); got["group_id"] != "turn-1" {
		t.Fatalf("grouped card payload = %v", got)
	}
	if got := withApprovalGroup(map[string]any{}, &store.Approval{}); len(got) != 0 {
		t.Fatalf("ungrouped card payload = %v, want unchanged", got)
	}
}
