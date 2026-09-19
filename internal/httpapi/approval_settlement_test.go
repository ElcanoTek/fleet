package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

type settlementStore struct {
	chatStore
	resolved []store.Approval
}

func (s *settlementStore) ListPendingApprovals(context.Context, string, string) ([]store.Approval, error) {
	return []store.Approval{{ID: "pending", ToolName: "bash", ArgsJSON: `{"command":"echo hi"}`}}, nil
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
