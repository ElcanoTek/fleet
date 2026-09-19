package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
)

type settlementStore struct {
	chatStore
	resolved []store.Approval
	pending  []store.Approval
}

func (s *settlementStore) ListPendingApprovals(context.Context, string, string) ([]store.Approval, error) {
	if s.pending != nil {
		return s.pending, nil
	}
	return []store.Approval{{ID: "pending", ToolName: "bash", ArgsJSON: `{"command":"echo hi"}`}}, nil
}

func (s *settlementStore) GetApproval(_ context.Context, user, id string) (*store.Approval, error) {
	for _, a := range s.pending {
		if a.ID == id && a.UserEmail == user {
			return &a, nil
		}
	}
	return nil, nil
}

func TestSingleApprovalReviewExcludesUnrelatedPendingBodies(t *testing.T) {
	st := &settlementStore{pending: []store.Approval{{ID: "small", ConversationID: "c", UserEmail: "u", Status: "pending", ToolName: "bash", ArgsJSON: `{"command":"echo hi"}`}}}
	for range 3 {
		st.pending = append(st.pending, store.Approval{ToolName: "mcp_sendgrid_send_email", ArgsJSON: `{"content":"` + strings.Repeat("<", 900<<10) + `"}`})
	}
	s := &Server{store: st}
	index := httptest.NewRecorder()
	s.handleConversationGet(index, httptest.NewRequest("GET", "/conversations/c?omit_history=1&settlement_only=1&approval_index=1", nil), "u", "c", &store.Conversation{ID: "c"})
	if index.Code != 200 || index.Body.Len() > 4096 {
		t.Fatalf("unbounded index: %d %d", index.Code, index.Body.Len())
	}
	for _, tc := range []struct {
		user, conv string
		status     int
	}{{"u", "c", 200}, {"u", "other", 404}, {"other", "c", 404}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/conversations/c?approval_id=small", nil)
		s.handleConversationGet(rec, req, tc.user, tc.conv, &store.Conversation{ID: tc.conv})
		if rec.Code != tc.status || rec.Body.Len() > 4096 {
			t.Fatalf("single-card response: status=%d bytes=%d", rec.Code, rec.Body.Len())
		}
		if tc.status == 200 && !strings.Contains(rec.Body.String(), `"approval_id":"small"`) {
			t.Fatal("requested card missing")
		}
	}
	st.pending[0].Status = "approved"
	st.pending[0].ResultText = "recorded result"
	rec := httptest.NewRecorder()
	s.handleConversationGet(rec, httptest.NewRequest("GET", "/conversations/c?approval_id=small", nil), "u", "c", &store.Conversation{ID: "c"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"result_text":"recorded result"`) {
		t.Fatalf("settled selector lost outcome: %d %s", rec.Code, rec.Body.String())
	}
}

type failedOutcomeStore struct{ *expiredClickFakeStore }

func (s *failedOutcomeStore) ClaimApproval(context.Context, string, string, string, string) (bool, error) {
	s.approval.Status = "approved"
	s.approval.ResultText = approvalExecutingSentinel
	return true, nil
}
func (s *failedOutcomeStore) SetApprovalResult(context.Context, string, string, string, bool) error {
	return errors.New("history unavailable")
}

func TestApprovalPersistenceFailureReportsUnknownOutcome(t *testing.T) {
	s := &Server{cfg: &config.Config{MockMode: true}, store: &failedOutcomeStore{&expiredClickFakeStore{approval: store.Approval{ID: "a", ConversationID: "c", ToolName: "bash", Status: "pending"}}}}
	req := httptest.NewRequest("POST", "/conversations/c/approvals/a", strings.NewReader(`{"approved":true}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c", "a")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["execution_unknown"] != true || body["is_err"] != nil {
		t.Fatalf("invented durable success: %s", rec.Body.String())
	}
	retry := httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/conversations/c/approvals/a", strings.NewReader(`{"approved":true}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u"))
	s.handleApproval(retry, req, "c", "a")
	if !strings.Contains(retry.Body.String(), `"execution_unknown":true`) || strings.Contains(retry.Body.String(), `"executing":true`) {
		t.Fatalf("retry regressed to running: %s", retry.Body.String())
	}
	get := httptest.NewRecorder()
	s.handleConversationApprovalGet(get, req, "u", "c", "a")
	if !strings.Contains(get.Body.String(), `"execution_unknown":true`) {
		t.Fatalf("reload lost unknown outcome: %s", get.Body.String())
	}
}

func TestWebReloadIncludesExecutingRowsOutsideResolvedLimit(t *testing.T) {
	st := &settlementStore{}
	for range 100 {
		st.resolved = append(st.resolved, store.Approval{ID: "completed", Status: "approved"})
	}
	s := &Server{store: st}
	rec := httptest.NewRecorder()
	s.handleConversationGet(rec, httptest.NewRequest("GET", "/conversations/c?omit_history=1", nil), "u", "c", &store.Conversation{ID: "c"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"approval_id":"running"`) || !strings.Contains(rec.Body.String(), `"executing":true`) {
		t.Fatalf("web reload lost running card: %d %s", rec.Code, rec.Body.String())
	}
}
func (s *settlementStore) ListResolvedApprovals(context.Context, string, string) ([]store.Approval, error) {
	return s.resolved, nil
}
func (s *settlementStore) ListExecutingApprovals(context.Context, string, string, string) ([]store.Approval, error) {
	return []store.Approval{{ID: "running", ToolName: "bash", Status: "approved", ResultText: approvalExecutingSentinel}}, nil
}
func (s *settlementStore) ListPendingMemoryProposalsForConversation(context.Context, string, string) ([]store.Memory, error) {
	return nil, nil
}

func TestTerminalSettlementExcludesLargeResolvedCards(t *testing.T) {
	st := &settlementStore{}
	for range 40 {
		st.resolved = append(st.resolved, store.Approval{ToolName: "mcp_sendgrid_send_email", Status: "approved", ArgsJSON: `{"content":"` + strings.Repeat("x", 1<<20) + `"}`})
	}
	s := &Server{store: st}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/conversations/c?omit_history=1&settlement_only=1", nil)
	s.handleConversationGet(rec, req, "u", "c", &store.Conversation{ID: "c"})
	if rec.Code != 200 || rec.Body.Len() > 4096 {
		t.Fatalf("settlement response status=%d bytes=%d", rec.Code, rec.Body.Len())
	}
	var body struct {
		Pending  []map[string]any `json:"pending_approvals"`
		Resolved []map[string]any `json:"resolved_approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Pending) != 1 || len(body.Resolved) != 1 || body.Resolved[0]["executing"] != true {
		t.Fatalf("lost actionable cards: %s", rec.Body.String())
	}
}

type suggestionReplayStore struct{ *expiredClickFakeStore }

func (s *suggestionReplayStore) Get(context.Context, string, string) (*store.Conversation, error) {
	return &store.Conversation{ID: "c", Model: "pinned/model"}, nil
}

func TestSuggestionReplayReturnsPinnedModel(t *testing.T) {
	s := &Server{store: &suggestionReplayStore{&expiredClickFakeStore{approval: store.Approval{
		ID: "a", ConversationID: "c", ToolName: "suggest_advanced_model", Status: "approved",
	}}}}
	req := httptest.NewRequest("POST", "/conversations/c/approvals/a", strings.NewReader(`{"approved":true}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u"))
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req, "c", "a")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"model":"pinned/model"`) {
		t.Fatalf("replay lost pin: %d %s", rec.Code, rec.Body.String())
	}
}
