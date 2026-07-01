package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// seedConv creates a conversation with a short user/assistant text history so a
// promote request has a non-empty transcript. Skips (via serverFixture) without
// a test DB.
func seedConv(t *testing.T, s *Server, user string) *store.Conversation {
	t.Helper()
	ctx := context.Background()
	conv, err := s.store.CreateConversation(ctx, user, "failed tasks", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := s.store.AppendHistory(ctx, conv.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"summarize scheduled tasks that failed today"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"3 failed: nightly-etl, db-backup, weekly-report."}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	return conv
}

func pendingScheduleApproval(t *testing.T, s *Server, user, convID string) *store.Approval {
	t.Helper()
	pend, err := s.store.ListPendingApprovals(context.Background(), user, convID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	for i := range pend {
		if pend[i].ToolName == "schedule_task" {
			return &pend[i]
		}
	}
	return nil
}

// TestPromoteToTask_StagesScheduleApproval drives the #455 endpoint: a synthesized
// proposal is staged as a schedule_task approval carrying the synthesized prompt
// + cron, and the response returns the approval-card payload + rationale.
func TestPromoteToTask_StagesScheduleApproval(t *testing.T) {
	s := serverFixture(t)
	const user = "alice@x.com"
	conv := seedConv(t, s, user)
	s.agent = &fakeTurnEngine{recurringProposal: &agent.RecurringTaskProposal{
		Name:      "Daily failed-task report",
		Prompt:    "Report scheduled tasks that failed in the last 24h.",
		Cron:      "0 9 * * *",
		Rationale: "a daily ops check keeps failures visible",
	}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/promote-to-task", nil)
	rec := httptest.NewRecorder()
	s.handlePromoteToTask(rec, req, conv.ID, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	appr := pendingScheduleApproval(t, s, user, conv.ID)
	if appr == nil {
		t.Fatal("expected a pending schedule_task approval to be staged")
	}
	var params tools.ScheduleTaskParams
	if err := json.Unmarshal([]byte(appr.ArgsJSON), &params); err != nil {
		t.Fatalf("staged args not ScheduleTaskParams: %v", err)
	}
	if params.Prompt != "Report scheduled tasks that failed in the last 24h." {
		t.Errorf("staged prompt = %q", params.Prompt)
	}
	if params.Cron != "0 9 * * *" {
		t.Errorf("staged cron = %q, want the synthesized value", params.Cron)
	}

	var body struct {
		Approval  map[string]any `json:"approval"`
		Rationale string         `json:"rationale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Approval["approval_id"] != appr.ID {
		t.Errorf("response approval_id = %v, want %q", body.Approval["approval_id"], appr.ID)
	}
	if body.Approval["tool"] != "schedule_task" {
		t.Errorf("response tool = %v", body.Approval["tool"])
	}
	if body.Rationale == "" {
		t.Error("expected a non-empty rationale in the response")
	}
}

// TestPromoteToTask_InvalidCronFallsBack verifies an unparseable synthesized cron
// is replaced with the safe daily default so the staged card always shows a
// valid recurring cadence.
func TestPromoteToTask_InvalidCronFallsBack(t *testing.T) {
	s := serverFixture(t)
	const user = "bob@x.com"
	conv := seedConv(t, s, user)
	s.agent = &fakeTurnEngine{recurringProposal: &agent.RecurringTaskProposal{
		Name:   "thing",
		Prompt: "do the thing",
		Cron:   "every day maybe", // not a cron
	}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/promote-to-task", nil)
	rec := httptest.NewRecorder()
	s.handlePromoteToTask(rec, req, conv.ID, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	appr := pendingScheduleApproval(t, s, user, conv.ID)
	if appr == nil {
		t.Fatal("expected a staged approval")
	}
	var params tools.ScheduleTaskParams
	_ = json.Unmarshal([]byte(appr.ArgsJSON), &params)
	if params.Cron != promoteFallbackCron {
		t.Errorf("invalid cron should fall back to %q, got %q", promoteFallbackCron, params.Cron)
	}
}

// TestPromoteToTask_EmptyConversation returns 422 when there's nothing to promote.
func TestPromoteToTask_EmptyConversation(t *testing.T) {
	s := serverFixture(t)
	const user = "carol@x.com"
	ctx := context.Background()
	conv, err := s.store.CreateConversation(ctx, user, "empty", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	s.agent = &fakeTurnEngine{recurringProposal: &agent.RecurringTaskProposal{Prompt: "x", Cron: "0 9 * * *"}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/promote-to-task", nil)
	rec := httptest.NewRecorder()
	s.handlePromoteToTask(rec, req, conv.ID, user)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty conversation should be 422, got %d", rec.Code)
	}
	if pendingScheduleApproval(t, s, user, conv.ID) != nil {
		t.Error("no approval should be staged for an empty conversation")
	}
}

// TestPromoteToTask_NotOwned returns 404 for a conversation the caller doesn't own.
func TestPromoteToTask_NotOwned(t *testing.T) {
	s := serverFixture(t)
	conv := seedConv(t, s, "owner@x.com")
	s.agent = &fakeTurnEngine{recurringProposal: &agent.RecurringTaskProposal{Prompt: "x", Cron: "0 9 * * *"}}

	req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/promote-to-task", nil)
	rec := httptest.NewRecorder()
	s.handlePromoteToTask(rec, req, conv.ID, "intruder@x.com")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign conversation should be 404, got %d", rec.Code)
	}
}
