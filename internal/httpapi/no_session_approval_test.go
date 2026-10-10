package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

// noSessionPolicy installs a bundle that lists create_deal in
// critical_tool_no_session_approval and leaves deploy_page with apply-all.
func noSessionPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes:          []string{"create_deal", "deploy_page"},
		CriticalToolNoSessionApproval: []string{"create_deal"},
	})
}

func postScope(s *Server, scope string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/conversations/c1/approvals/ap1",
		strings.NewReader(`{"approved":true,"scope":"`+scope+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c1", "ap1")
	return rec
}

// The approve POST refuses a session or pattern scope for a no-session tool
// with a 400 that says why, and claims nothing: the card stays pending for a
// per-call decision. A tool without the rule still takes the scope.
func TestHandleApproval_RefusesSessionScopeForNoSessionTool(t *testing.T) {
	noSessionPolicy(t)
	for _, scope := range []string{"session", "pattern", "pattern:deal_name=*", " session "} {
		t.Run(scope, func(t *testing.T) {
			st := &claimStore{approval: store.Approval{ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
				ToolName: "mcp_deals_client_a_create_deal", ArgsJSON: `{"deal_name":"x"}`, Status: "pending"}}
			s := &Server{store: st, sessionApprovals: NewSessionApprovalRegistry()}
			rec := postScope(s, scope)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "its own decision for each call") {
				t.Fatalf("got %d %q, want 400 naming the per-call rule", rec.Code, rec.Body.String())
			}
			if claims, _ := st.counts(); claims != 0 || st.approval.Status != "pending" {
				t.Fatalf("claims=%d status=%q: a refused scope must leave the card pending", claims, st.approval.Status)
			}
			if _, ok := s.sessionApprovals.Match("c1", "mcp_deals_client_a_create_deal", `{"deal_name":"x"}`); ok {
				t.Fatal("a refused scope registered a session policy")
			}
		})
	}
	// Reject with a session scope is refused the same way.
	st := &claimStore{approval: store.Approval{ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_deals_create_deal", Status: "pending"}}
	s := &Server{store: st, sessionApprovals: NewSessionApprovalRegistry()}
	req := httptest.NewRequest(http.MethodPost, "/conversations/c1/approvals/ap1", strings.NewReader(`{"approved":false,"scope":"session"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c1", "ap1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deny-all for a no-session tool: got %d, want 400", rec.Code)
	}
}

// A settled card still answers its recorded outcome (idempotent), whatever
// scope a retry sends: the refusal only guards a pending decision.
func TestHandleApproval_NoSessionRefusalKeepsSettledOutcome(t *testing.T) {
	noSessionPolicy(t)
	st := &claimStore{approval: store.Approval{ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: "mcp_deals_create_deal", Status: "rejected", ResultText: "User declined this action."}}
	s := &Server{store: st}
	if rec := postScope(s, "session"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"rejected"`) {
		t.Fatalf("got %d %s, want the recorded outcome", rec.Code, rec.Body.String())
	}
}

// Defense in depth: maybeRegisterSessionPolicy never records a policy for a
// no-session tool, and still does for any other.
func TestMaybeRegisterSessionPolicy_SkipsNoSessionTool(t *testing.T) {
	noSessionPolicy(t)
	s := New(&config.Config{}, &fakeEngine{}, nil)
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_deals_create_deal", approvalRequest{Approved: true, Scope: "session"})
	if _, ok := s.sessionApprovals.Match("c1", "mcp_deals_create_deal", `{}`); ok {
		t.Fatal("a session policy was registered for a no-session tool")
	}
	s.maybeRegisterSessionPolicy("c1", "u@e.com", "mcp_pages_deploy_page", approvalRequest{Approved: true, Scope: "session"})
	if _, ok := s.sessionApprovals.Match("c1", "mcp_pages_deploy_page", `{}`); !ok {
		t.Fatal("apply-all must keep working for a tool without the rule")
	}
}

// stageStore is the minimum chatStore Stage needs to create a card.
type stageStore struct {
	chatStore
	created *store.Approval
}

func (s *stageStore) SupersedePendingApprovals(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (s *stageStore) CreateApproval(_ context.Context, conv, user, tool, call, args string, expiry int64, _ store.ApprovalSeat) (*store.Approval, error) {
	s.created = &store.Approval{ID: "ap-new", ConversationID: conv, UserEmail: user, ToolName: tool, ToolCallID: call, ArgsJSON: args, ExpiresAt: expiry, Status: "pending"}
	return s.created, nil
}

type eventSink struct {
	names    []string
	payloads []any
}

func (e *eventSink) Emit(name string, payload any) {
	e.names = append(e.names, name)
	e.payloads = append(e.payloads, payload)
}

// Stage ignores a pre-existing session approve-all (registered before the
// rule, or by any path that skipped the POST refusal) for a no-session tool:
// the call stages its own card, and the card says apply-all is off. Another
// tool's session policy still short-circuits as before.
func TestStage_IgnoresSessionPolicyForNoSessionTool(t *testing.T) {
	noSessionPolicy(t)
	reg := NewSessionApprovalRegistry()
	reg.Register("c1", "mcp_deals_create_deal", SessionApprovalPolicy{Mode: "approve"})
	reg.Register("c1", "mcp_pages_deploy_page", SessionApprovalPolicy{Mode: "approve"})
	st := &stageStore{}
	sink := &eventSink{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c1", userEmail: "u@e.com", sink: sink, sessionRegistry: reg}

	id, err := a.Stage("mcp_deals_create_deal", "call-1", `{"deal_name":"x"}`)
	if err != nil || id != "ap-new" || st.created == nil {
		t.Fatalf("Stage = %q, %v; want a staged card, not the pre-approved sentinel", id, err)
	}
	if len(sink.names) != 1 || sink.names[0] != "tool.approval_required" {
		t.Fatalf("events = %v, want exactly tool.approval_required", sink.names)
	}
	ev, _ := sink.payloads[0].(map[string]any)
	if ev["no_session_approval"] != true {
		t.Fatalf("approval_required no_session_approval = %v, want true", ev["no_session_approval"])
	}

	id, err = a.Stage("mcp_pages_deploy_page", "call-2", `{"slug":"q3"}`)
	if err != nil || id != agentcore.PreApprovedSentinel {
		t.Fatalf("Stage = %q, %v; another tool's apply-all must still short-circuit", id, err)
	}
}

// pending_approvals and the live event share approvalClientFields, so both
// carry the flag: true only for a matching tool.
func TestApprovalClientFields_NoSessionApproval(t *testing.T) {
	noSessionPolicy(t)
	if got := approvalClientFields("mcp_deals_create_deal", `{}`, "c1")["no_session_approval"]; got != true {
		t.Errorf("create_deal no_session_approval = %v, want true", got)
	}
	if got := approvalClientFields("mcp_pages_deploy_page", `{}`, "c1")["no_session_approval"]; got != false {
		t.Errorf("deploy_page no_session_approval = %v, want false", got)
	}
}
