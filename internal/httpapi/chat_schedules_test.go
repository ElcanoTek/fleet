package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

type scheduleFakeStore struct {
	*fakeChatStore
	schedules map[string]store.ChatSchedule
}

func newScheduleFakeStore() *scheduleFakeStore {
	return &scheduleFakeStore{
		fakeChatStore: newFakeChatStore(),
		schedules:     map[string]store.ChatSchedule{},
	}
}

func (s *scheduleFakeStore) SetAutoApprove(_ context.Context, userEmail, convID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[convID]
	if !ok || c.UserEmail != userEmail {
		return store.ErrConversationNotFound
	}
	c.AutoApprove = enabled
	return nil
}

func (s *scheduleFakeStore) CreateChatSchedule(_ context.Context, in store.ChatScheduleInput) (*store.ChatSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[in.ConversationID]
	if !ok || c.UserEmail != in.UserEmail {
		return nil, store.ErrConversationNotFound
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, store.ErrInvalidChatSchedule
	}
	now := time.Now().Unix()
	kind := store.ChatScheduleKindPatrol
	nextRun := now + 3600
	if in.DurationSeconds > 0 {
		kind = store.ChatScheduleKindTimer
		nextRun = now + int64(in.DurationSeconds)
	}
	// Upsert existing active/paused recurring patrol when !AllowMultiple.
	if in.CronExpr != "" && !in.AllowMultiple {
		for id, existing := range s.schedules {
			if existing.ConversationID == in.ConversationID && existing.CronExpr != "" &&
				(existing.Status == store.ChatScheduleStatusActive || existing.Status == store.ChatScheduleStatusPaused) {
				existing.Title = in.Title
				existing.Prompt = in.Prompt
				existing.CronExpr = in.CronExpr
				existing.Status = store.ChatScheduleStatusActive
				existing.UpdatedAt = now
				s.schedules[id] = existing
				cp := existing
				return &cp, nil
			}
		}
	}
	sc := store.ChatSchedule{
		ID:              "sched-1",
		ConversationID:  in.ConversationID,
		UserEmail:       in.UserEmail,
		Title:           in.Title,
		Prompt:          in.Prompt,
		Kind:            kind,
		CronExpr:        in.CronExpr,
		Timezone:        "UTC",
		DurationSeconds: in.DurationSeconds,
		Status:          store.ChatScheduleStatusActive,
		NextRunAt:       nextRun,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.schedules[sc.ID] = sc
	return &sc, nil
}

func (s *scheduleFakeStore) ListChatSchedules(_ context.Context, userEmail, convID string, includeTerminal bool) ([]store.ChatSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.ChatSchedule
	for _, sc := range s.schedules {
		if sc.ConversationID != convID || sc.UserEmail != userEmail {
			continue
		}
		if !includeTerminal && sc.Status != store.ChatScheduleStatusActive && sc.Status != store.ChatScheduleStatusPaused {
			continue
		}
		out = append(out, sc)
	}
	return out, nil
}

func (s *scheduleFakeStore) UpdateChatSchedule(_ context.Context, userEmail, convID, scheduleID string, patch store.ChatSchedulePatch) (*store.ChatSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.schedules[scheduleID]
	if !ok || sc.ConversationID != convID || sc.UserEmail != userEmail {
		return nil, store.ErrChatScheduleNotFound
	}
	if patch.Title != nil {
		sc.Title = *patch.Title
	}
	if patch.Prompt != nil {
		sc.Prompt = *patch.Prompt
	}
	if patch.CronExpr != nil {
		sc.CronExpr = *patch.CronExpr
	}
	if patch.Status != nil {
		sc.Status = *patch.Status
	}
	sc.UpdatedAt = time.Now().Unix()
	s.schedules[scheduleID] = sc
	return &sc, nil
}

func (s *scheduleFakeStore) CancelChatSchedule(ctx context.Context, userEmail, convID, scheduleID string) (*store.ChatSchedule, error) {
	st := store.ChatScheduleStatusCancelled
	return s.UpdateChatSchedule(ctx, userEmail, convID, scheduleID, store.ChatSchedulePatch{Status: &st})
}

func (s *scheduleFakeStore) TriggerChatScheduleNow(ctx context.Context, userEmail, convID, scheduleID string, _ int) (*store.ClaimedChatSchedule, error) {
	s.mu.Lock()
	sc, ok := s.schedules[scheduleID]
	if !ok || sc.ConversationID != convID || sc.UserEmail != userEmail {
		s.mu.Unlock()
		return nil, store.ErrChatScheduleNotFound
	}
	sc.RunCount++
	now := time.Now().Unix()
	sc.LastRunAt = &now
	s.schedules[scheduleID] = sc
	s.mu.Unlock()

	row, _, err := s.EnqueueInput(ctx, store.InputQueueRow{
		ID:             "q-patrol-1",
		ConversationID: convID,
		UserEmail:      userEmail,
		ClientInputID:  store.ChatPatrolInputKeyPrefix + scheduleID + ":1",
		Message:        store.ChatPatrolInputText(sc, sc.RunCount),
		Attachments:    "[]",
		Mode:           store.InputModePatrol,
	})
	if err != nil {
		return nil, err
	}
	return &store.ClaimedChatSchedule{Schedule: sc, Input: row}, nil
}

func (s *scheduleFakeStore) ClaimDueChatSchedules(ctx context.Context, _ time.Time, maxPending int) ([]store.ClaimedChatSchedule, error) {
	s.mu.Lock()
	var ids []struct{ user, conv, id string }
	for _, sc := range s.schedules {
		if sc.Status == store.ChatScheduleStatusActive {
			ids = append(ids, struct{ user, conv, id string }{sc.UserEmail, sc.ConversationID, sc.ID})
		}
	}
	s.mu.Unlock()

	var out []store.ClaimedChatSchedule
	for _, item := range ids {
		claimed, err := s.TriggerChatScheduleNow(ctx, item.user, item.conv, item.id, maxPending)
		if err != nil {
			return out, err
		}
		out = append(out, *claimed)
	}
	return out, nil
}

type patrolRecordingEngine struct {
	fakeEngine
	lastInputKind   string
	lastUserMessage string
	invokeTool      bool
}

func (e *patrolRecordingEngine) RunTurn(ctx context.Context, in TurnInput, sink agent.EventSink) (*TurnResult, error) {
	e.mu.Lock()
	e.lastInputKind = in.InputKind
	e.lastUserMessage = in.UserMessage
	shouldCallTool := e.invokeTool
	e.mu.Unlock()

	if shouldCallTool {
		tool := tools.NewChatPatrolTool()
		_, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    "call_patrol_1",
			Name:  tools.ChatPatrolToolName,
			Input: `{"action":"create","title":"2h Deal Patrol","prompt":"Check deal pacing and margin.","cron":"0 */2 * * *","auto_approve":true}`,
		})
		if err != nil {
			return nil, err
		}
	}
	return e.fakeEngine.RunTurn(ctx, in, sink)
}

func TestChatScheduleRoutes_AndPatrolDrain(t *testing.T) {
	st := newScheduleFakeStore()
	eng := &patrolRecordingEngine{}
	srv := newDefaultChatServer(t, eng, st)
	defer srv.StopBackground()
	h := srv.Routes()

	conv, err := st.CreateConversation(context.Background(), "trader@elcanotek.com", "Deal Patrol Thread", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	doJSON := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("X-Chat-Server-Token", "tok")
		req.Header.Set("X-User-Email", "trader@elcanotek.com")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1. POST /conversations/{id}/schedules creates a patrol and enables auto_approve.
	rec := doJSON(http.MethodPost, "/conversations/"+conv.ID+"/schedules", `{
		"title": "Hourly Deal & Margin Patrol",
		"prompt": "Check all active deals for under-pacing and update the dashboard.",
		"cron": "0 */2 * * *",
		"timezone": "UTC",
		"auto_approve": true
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /schedules status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var createResp struct {
		Schedule    store.ChatSchedule   `json:"schedule"`
		Schedules   []store.ChatSchedule `json:"schedules"`
		AutoApprove bool                 `json:"auto_approve"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("unmarshal POST response: %v", err)
	}
	if !createResp.AutoApprove {
		t.Errorf("expected auto_approve=true after schedule creation")
	}
	if createResp.Schedule.Title != "Hourly Deal & Margin Patrol" || createResp.Schedule.Kind != "patrol" {
		t.Errorf("unexpected schedule: %+v", createResp.Schedule)
	}

	// 2. PATCH /conversations/{id}/schedules/{scheduleId} pauses and resumes.
	rec = doJSON(http.MethodPatch, "/conversations/"+conv.ID+"/schedules/"+createResp.Schedule.ID, `{"action":"pause"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH pause status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(http.MethodPatch, "/conversations/"+conv.ID+"/schedules/"+createResp.Schedule.ID, `{"action":"resume"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH resume status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 3. POST /conversations/{id}/schedules/{scheduleId}/run-now triggers a patrol turn
	// with InputKindChatPatrol.
	rec = doJSON(http.MethodPost, "/conversations/"+conv.ID+"/schedules/"+createResp.Schedule.ID+"/run-now", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST run-now status = %d, body = %s", rec.Code, rec.Body.String())
	}
	srv.DrainTurns(context.Background())

	eng.mu.Lock()
	gotKind := eng.lastInputKind
	gotMsg := eng.lastUserMessage
	eng.mu.Unlock()
	if gotKind != agent.InputKindChatPatrol {
		t.Errorf("drained patrol turn InputKind = %q, want %q", gotKind, agent.InputKindChatPatrol)
	}
	if !strings.Contains(gotMsg, "Hourly Deal & Margin Patrol") {
		t.Errorf("drained patrol turn message = %q, want schedule title", gotMsg)
	}

	// 4. DELETE /conversations/{id}/schedules/{scheduleId} cancels the patrol.
	rec = doJSON(http.MethodDelete, "/conversations/"+conv.ID+"/schedules/"+createResp.Schedule.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE schedule status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestChatPatrolTool_InvokedInsideTurn(t *testing.T) {
	st := newScheduleFakeStore()
	eng := &patrolRecordingEngine{invokeTool: true}
	srv := newDefaultChatServer(t, eng, st)
	defer srv.StopBackground()
	h := srv.Routes()

	conv, err := st.CreateConversation(context.Background(), "trader@elcanotek.com", "Chat", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"conversation_id": conv.ID,
		"message":         "Please patrol my deals every 2 hours in this chat.",
	})
	req := httptest.NewRequest(http.MethodPost, "/chat", bytes.NewReader(body))
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "trader@elcanotek.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	srv.DrainTurns(context.Background())

	// Verify the schedule was created and patrol.updated was emitted on the SSE stream,
	// while the model-callable chat_patrol tool did NOT elevate conv.AutoApprove without user consent.
	if !strings.Contains(rec.Body.String(), "event: patrol.updated") {
		t.Errorf("expected SSE stream to contain patrol.updated event, got:\n%s", rec.Body.String())
	}
	list, _ := st.ListChatSchedules(context.Background(), "trader@elcanotek.com", conv.ID, false)
	if len(list) != 1 || list[0].Title != "2h Deal Patrol" {
		t.Errorf("expected 1 created patrol schedule, got %+v", list)
	}
	updatedConv, _ := st.Get(context.Background(), "trader@elcanotek.com", conv.ID)
	if updatedConv != nil && updatedConv.AutoApprove {
		t.Errorf("model-invoked chat_patrol must not enable conversation auto_approve without explicit user action")
	}
}
