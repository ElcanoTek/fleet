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
	"github.com/ElcanoTek/fleet/internal/sched/models"
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
		if sc.ConversationID != convID || (userEmail != "" && sc.UserEmail != userEmail) {
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

func (s *scheduleFakeStore) DeliverChatScheduleFire(ctx context.Context, fire models.ChatScheduleFire, _ int) (models.ChatScheduleDelivery, error) {
	s.mu.Lock()
	sc, ok := s.schedules[fire.ScheduleID]
	s.mu.Unlock()
	if !ok {
		return models.ChatScheduleDelivery{Outcome: models.ChatScheduleGone}, nil
	}
	row, _, err := s.EnqueueInput(ctx, store.InputQueueRow{
		ID:             "q-patrol-1",
		ConversationID: fire.ConversationID,
		UserEmail:      fire.UserEmail,
		ClientInputID:  store.ChatScheduleInputKey(fire),
		Message:        store.ChatPatrolInputText(sc, fire.RunNumber),
		Attachments:    "[]",
		Mode:           store.InputModePatrol,
	})
	if err != nil {
		return models.ChatScheduleDelivery{}, err
	}
	return models.ChatScheduleDelivery{Outcome: models.ChatScheduleQueued, InputID: row.ID}, nil
}

// fakeChatScheduleEngine stands in for the scheduler: Run now claims the
// fake store's row, delivers through the server and settles the run count.
type fakeChatScheduleEngine struct {
	st    *scheduleFakeStore
	srv   *Server
	wakes []time.Time
}

func (e *fakeChatScheduleEngine) RunChatScheduleNow(ctx context.Context, scheduleID string) (models.ChatScheduleDelivery, error) {
	e.st.mu.Lock()
	sc, ok := e.st.schedules[scheduleID]
	e.st.mu.Unlock()
	if !ok {
		return models.ChatScheduleDelivery{}, store.ErrChatScheduleNotFound
	}
	fire := models.ChatScheduleFire{
		ScheduleID: sc.ID, ConversationID: sc.ConversationID, UserEmail: sc.UserEmail,
		Title: sc.Title, Prompt: sc.Prompt, CronExpr: sc.CronExpr, RunNumber: sc.RunCount + 1,
		Trigger: models.ChatScheduleTriggerManual, ClaimedAt: time.Now(),
	}
	d, err := e.srv.DeliverChatScheduleFire(ctx, fire)
	if err != nil {
		return d, err
	}
	if d.Outcome == models.ChatScheduleQueued {
		e.st.mu.Lock()
		sc.RunCount++
		now := time.Now().Unix()
		sc.LastRunAt = &now
		e.st.schedules[scheduleID] = sc
		e.st.mu.Unlock()
	}
	return d, nil
}

func (e *fakeChatScheduleEngine) WakeChatSchedulesAt(t time.Time) { e.wakes = append(e.wakes, t) }

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
	t.Parallel()

	st := newScheduleFakeStore()
	eng := &patrolRecordingEngine{}
	srv := newDefaultChatServer(t, eng, st)
	srv.chatSchedules = &fakeChatScheduleEngine{st: st, srv: srv}
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
	t.Parallel()

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
