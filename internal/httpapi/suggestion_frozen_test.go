package httpapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

type frozenSuggestionStore struct {
	chatStore
	approval *store.Approval
	model    string
}

func (s *frozenSuggestionStore) Get(context.Context, string, string) (*store.Conversation, error) {
	return &store.Conversation{ID: "c", Model: s.model}, nil
}
func (s *frozenSuggestionStore) LatestApprovalByTool(context.Context, string, string) (*store.Approval, error) {
	return nil, nil
}
func (s *frozenSuggestionStore) SupersedePendingApprovals(context.Context, string, string) (int64, error) {
	return 0, nil
}
func (s *frozenSuggestionStore) CreateApproval(_ context.Context, conv, user, tool, call, args string, expiry int64, _ store.ApprovalSeat) (*store.Approval, error) {
	s.approval = &store.Approval{ID: "a", ConversationID: conv, UserEmail: user, ToolName: tool, ToolCallID: call, ArgsJSON: args, ExpiresAt: expiry, Status: "pending"}
	return s.approval, nil
}
func (s *frozenSuggestionStore) ClaimApprovalAndSetModel(_ context.Context, _, _, _, _, model string) (bool, error) {
	s.model = model
	return true, nil
}
func (s *frozenSuggestionStore) AppendHistory(context.Context, string, []agent.HistoryEntry) ([]int64, error) {
	return nil, nil
}

func TestSuggestionFreezesConfiguredTargetAtStaging(t *testing.T) {
	original := agentcore.CurrentAdvancedModel()
	t.Cleanup(func() { agentcore.SetAdvancedModel(original) })
	agentcore.SetAdvancedModel("model/approved")
	st := &frozenSuggestionStore{}
	a := &approvalStager{ctx: context.Background(), store: st, conversationID: "c", userEmail: "u", sink: &recordingSink{}}
	if _, _, err := a.StageSuggestion("complex task"); err != nil {
		t.Fatal(err)
	}
	agentcore.SetAdvancedModel("model/changed")
	fields := approvalClientFields(st.approval.ToolName, st.approval.ArgsJSON, "c")
	summary := fields["summary"].(map[string]any)
	frozen := fields["frozen_args"].(map[string]any)
	if summary["recommend_model"] != "model/approved" || frozen["args"].(map[string]any)["recommend_model"] != "model/approved" {
		t.Fatalf("review target drifted: %v", fields)
	}
	s := &Server{store: st}
	rec := httptest.NewRecorder()
	s.handleSuggestAdvancedApproval(context.Background(), rec, httptest.NewRequest("POST", "/", nil), "u", st.approval, approvalRequest{Approved: true})
	if rec.Code != 200 || st.model != "model/approved" {
		t.Fatalf("execution target drifted: %d %q", rec.Code, st.model)
	}
}

func TestLegacySuggestionWithoutTargetFailsClosed(t *testing.T) {
	const raw = `{"reason":"legacy"}`
	fields := approvalClientFields("suggest_advanced_model", raw, "c")
	if fields["frozen_args"].(map[string]any)["complete"] != false {
		t.Fatal("legacy card claimed complete review")
	}
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleSuggestAdvancedApproval(context.Background(), rec, httptest.NewRequest("POST", "/", nil), "u", &store.Approval{ArgsJSON: raw}, approvalRequest{Approved: true})
	if rec.Code != 409 {
		t.Fatalf("legacy approval = %d", rec.Code)
	}
}
