package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// SweepDueChatSchedules claims any active in-chat patrols or timers whose
// next_run_at has arrived and enqueues their turns into chat_input_queue, then
// kicks each conversation's queue drain.
func (s *Server) SweepDueChatSchedules(ctx context.Context) (int, error) {
	if s == nil || s.store == nil || s.shuttingDown.Load() || UpdateDrainActive() {
		return 0, nil
	}
	claimed, err := s.store.ClaimDueChatSchedules(ctx, time.Now(), maxPendingInputs)
	for _, item := range claimed {
		log.Printf("audit: chat schedule %q (run #%d) queued for conversation %q as input %q",
			item.Schedule.ID, item.Schedule.RunCount, item.Schedule.ConversationID, item.Input.ID)
		s.emitQueueUpdate(ctx, item.Input.UserEmail, item.Schedule.ConversationID)
		s.maybeDrainQueue(item.Schedule.ConversationID)
		autoApprove := false
		if conv, cerr := s.store.Get(ctx, item.Input.UserEmail, item.Schedule.ConversationID); cerr == nil && conv != nil {
			autoApprove = conv.AutoApprove
		}
		s.emitPatrolUpdate(ctx, item.Input.UserEmail, item.Schedule.ConversationID, nil, autoApprove)
	}
	if ql, ok := s.store.(interface {
		ListQueuedPatrolConversations(context.Context) ([]string, error)
	}); ok {
		if convIDs, qerr := ql.ListQueuedPatrolConversations(ctx); qerr == nil {
			for _, convID := range convIDs {
				s.maybeDrainQueue(convID)
			}
		}
	}
	return len(claimed), err
}

// armShortScheduleTimer schedules a tracked background wakeup when a newly
// created or updated schedule fires within 5 minutes so short one-shot timers
// (and integration tests) fire right at next_run_at without waiting on the
// daemon's periodic sweep ticker.
func (s *Server) armShortScheduleTimer(nextRunAt int64) {
	if s == nil || nextRunAt <= 0 {
		return
	}
	delay := time.Until(time.Unix(nextRunAt, 0))
	if delay < 50*time.Millisecond {
		delay = 50 * time.Millisecond
	}
	if delay > 5*time.Minute {
		return
	}
	s.background.After("httpapi.chat_schedule_due", delay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := s.SweepDueChatSchedules(ctx); err != nil {
			log.Printf("chat schedule timer sweep: %v", err)
		}
	})
}

func toChatPatrolScheduleView(sc store.ChatSchedule) tools.ChatPatrolScheduleView {
	nextISO := ""
	if sc.NextRunAt > 0 {
		nextISO = time.Unix(sc.NextRunAt, 0).UTC().Format(time.RFC3339)
	}
	return tools.ChatPatrolScheduleView{
		ID:              sc.ID,
		ConversationID:  sc.ConversationID,
		Title:           sc.Title,
		Prompt:          sc.Prompt,
		Kind:            sc.Kind,
		Cron:            sc.CronExpr,
		Timezone:        sc.Timezone,
		DurationSeconds: sc.DurationSeconds,
		Status:          sc.Status,
		NextRunAt:       sc.NextRunAt,
		NextRunISO:      nextISO,
		LastRunAt:       sc.LastRunAt,
		RunCount:        sc.RunCount,
		MaxRuns:         sc.MaxRuns,
	}
}

func toChatPatrolScheduleViews(list []store.ChatSchedule) []tools.ChatPatrolScheduleView {
	out := make([]tools.ChatPatrolScheduleView, 0, len(list))
	for _, item := range list {
		out = append(out, toChatPatrolScheduleView(item))
	}
	return out
}

func (s *Server) emitPatrolUpdate(ctx context.Context, user, convID string, buf *turnBuffer, autoApprove bool) {
	if s.store == nil {
		return
	}
	list, err := s.store.ListChatSchedules(ctx, user, convID, false)
	if err != nil {
		return
	}
	payload := map[string]any{
		"schedules":    list,
		"auto_approve": autoApprove,
	}
	if buf != nil {
		buf.Emit("patrol.updated", payload)
		return
	}
	if entry, ok := s.getInflight(convID); ok && entry.buf != nil {
		entry.buf.Emit("patrol.updated", payload)
	}
}

// newChatPatrolHandler constructs the per-turn callback injected into turnCtx
// for the interactive chat_patrol tool.
func (s *Server) newChatPatrolHandler(conv *store.Conversation, user string, buf *turnBuffer) tools.ChatPatrolHandler {
	return func(ctx context.Context, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
		if s.store == nil || conv == nil {
			return nil, errors.New("chat schedule store is not configured")
		}
		// Refresh conv.AutoApprove from the store so the returned view reflects
		// any user toggle without allowing the model-callable tool to elevate
		// AutoApprove on its own.
		if fresh, err := s.store.Get(ctx, user, conv.ID); err == nil && fresh != nil {
			conv.AutoApprove = fresh.AutoApprove
		}

		switch params.Action {
		case tools.ChatPatrolActionList:
			return s.chatPatrolList(ctx, conv, user, params)
		case tools.ChatPatrolActionCreate:
			return s.chatPatrolCreate(ctx, conv, user, buf, params)
		case tools.ChatPatrolActionUpdate:
			return s.chatPatrolUpdate(ctx, conv, user, buf, params)
		case tools.ChatPatrolActionPause, tools.ChatPatrolActionResume:
			return s.chatPatrolTogglePause(ctx, conv, user, buf, params)
		case tools.ChatPatrolActionCancel:
			return s.chatPatrolCancel(ctx, conv, user, buf, params)
		default:
			return nil, fmt.Errorf("unsupported action %q", params.Action)
		}
	}
}

func (s *Server) resolveChatScheduleTargetID(ctx context.Context, user, convID, explicit string) (string, error) {
	id := strings.TrimSpace(explicit)
	if id != "" {
		return id, nil
	}
	active, err := s.store.ListChatSchedules(ctx, user, convID, false)
	if err != nil {
		return "", err
	}
	if len(active) == 0 {
		return "", errors.New("this conversation has no active or paused patrol/timer schedules")
	}
	if len(active) == 1 {
		return active[0].ID, nil
	}
	return "", fmt.Errorf("conversation has %d schedules; specify schedule_id", len(active))
}

func (s *Server) chatPatrolList(ctx context.Context, conv *store.Conversation, user string, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
	list, err := s.store.ListChatSchedules(ctx, user, conv.ID, false)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%d active/paused schedule(s) in this conversation.", len(list))
	if len(list) == 0 {
		msg = "No active or paused schedules in this conversation."
	}
	return &tools.ChatPatrolResult{
		Status:      "ok",
		Action:      params.Action,
		Message:     msg,
		AutoApprove: conv.AutoApprove,
		Schedules:   toChatPatrolScheduleViews(list),
	}, nil
}

func (s *Server) chatPatrolCreate(ctx context.Context, conv *store.Conversation, user string, buf *turnBuffer, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
	maxRuns := 0
	if params.MaxRuns != nil && *params.MaxRuns > 0 {
		maxRuns = *params.MaxRuns
	}
	sched, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
		ConversationID:  conv.ID,
		UserEmail:       user,
		Title:           params.Title,
		Prompt:          params.Prompt,
		CronExpr:        params.Cron,
		Timezone:        params.Timezone,
		DurationSeconds: params.DurationSeconds,
		MaxRuns:         maxRuns,
		AllowMultiple:   params.AllowMultiple,
	})
	if err != nil {
		return nil, err
	}
	s.armShortScheduleTimer(sched.NextRunAt)
	s.emitPatrolUpdate(ctx, user, conv.ID, buf, conv.AutoApprove)
	list, _ := s.store.ListChatSchedules(ctx, user, conv.ID, false)
	view := toChatPatrolScheduleView(*sched)
	msg := fmt.Sprintf("Scheduled %s %q (id=%s, next run at %s).", view.Kind, view.Title, view.ID, view.NextRunISO)
	if !conv.AutoApprove {
		msg += " Tip: ask the user to enable ⚡ Auto-approve in the Patrol bar or composer so unattended patrol runs do not pause on tool approval cards."
	}
	return &tools.ChatPatrolResult{
		Status:      "ok",
		Action:      params.Action,
		Message:     msg,
		AutoApprove: conv.AutoApprove,
		Schedule:    &view,
		Schedules:   toChatPatrolScheduleViews(list),
	}, nil
}

func buildChatPatrolUpdatePatch(params tools.ChatPatrolParams) store.ChatSchedulePatch {
	var patch store.ChatSchedulePatch
	if t := strings.TrimSpace(params.Title); t != "" {
		patch.Title = &t
	}
	if p := strings.TrimSpace(params.Prompt); p != "" {
		patch.Prompt = &p
	}
	if c := strings.TrimSpace(params.Cron); c != "" {
		patch.CronExpr = &c
	}
	if tz := strings.TrimSpace(params.Timezone); tz != "" {
		patch.Timezone = &tz
	}
	if params.DurationSeconds > 0 {
		d := params.DurationSeconds
		patch.DurationSeconds = &d
	}
	if params.MaxRuns != nil && *params.MaxRuns >= 0 {
		m := *params.MaxRuns
		patch.MaxRuns = &m
	}
	return patch
}

func (s *Server) chatPatrolUpdate(ctx context.Context, conv *store.Conversation, user string, buf *turnBuffer, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
	targetID, err := s.resolveChatScheduleTargetID(ctx, user, conv.ID, params.ScheduleID)
	if err != nil {
		return nil, err
	}
	patch := buildChatPatrolUpdatePatch(params)
	sched, err := s.store.UpdateChatSchedule(ctx, user, conv.ID, targetID, patch)
	if err != nil {
		return nil, err
	}
	s.armShortScheduleTimer(sched.NextRunAt)
	s.emitPatrolUpdate(ctx, user, conv.ID, buf, conv.AutoApprove)
	list, _ := s.store.ListChatSchedules(ctx, user, conv.ID, false)
	view := toChatPatrolScheduleView(*sched)
	return &tools.ChatPatrolResult{
		Status:      "ok",
		Action:      params.Action,
		Message:     fmt.Sprintf("Updated %s %q (id=%s, next run at %s).", view.Kind, view.Title, view.ID, view.NextRunISO),
		AutoApprove: conv.AutoApprove,
		Schedule:    &view,
		Schedules:   toChatPatrolScheduleViews(list),
	}, nil
}

func (s *Server) chatPatrolTogglePause(ctx context.Context, conv *store.Conversation, user string, buf *turnBuffer, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
	targetID, err := s.resolveChatScheduleTargetID(ctx, user, conv.ID, params.ScheduleID)
	if err != nil {
		return nil, err
	}
	status := store.ChatScheduleStatusPaused
	if params.Action == tools.ChatPatrolActionResume {
		status = store.ChatScheduleStatusActive
	}
	sched, err := s.store.UpdateChatSchedule(ctx, user, conv.ID, targetID, store.ChatSchedulePatch{Status: &status})
	if err != nil {
		return nil, err
	}
	if status == store.ChatScheduleStatusActive {
		s.armShortScheduleTimer(sched.NextRunAt)
	}
	s.emitPatrolUpdate(ctx, user, conv.ID, buf, conv.AutoApprove)
	list, _ := s.store.ListChatSchedules(ctx, user, conv.ID, false)
	view := toChatPatrolScheduleView(*sched)
	return &tools.ChatPatrolResult{
		Status:      "ok",
		Action:      params.Action,
		Message:     fmt.Sprintf("Schedule %q (id=%s) is now %s.", view.Title, view.ID, view.Status),
		AutoApprove: conv.AutoApprove,
		Schedule:    &view,
		Schedules:   toChatPatrolScheduleViews(list),
	}, nil
}

func (s *Server) chatPatrolCancel(ctx context.Context, conv *store.Conversation, user string, buf *turnBuffer, params tools.ChatPatrolParams) (*tools.ChatPatrolResult, error) {
	if strings.EqualFold(strings.TrimSpace(params.ScheduleID), "all") {
		active, err := s.store.ListChatSchedules(ctx, user, conv.ID, false)
		if err != nil {
			return nil, err
		}
		for _, item := range active {
			if _, cerr := s.store.CancelChatSchedule(ctx, user, conv.ID, item.ID); cerr != nil && !errors.Is(cerr, store.ErrChatScheduleNotFound) {
				return nil, cerr
			}
		}
		s.emitPatrolUpdate(ctx, user, conv.ID, buf, conv.AutoApprove)
		return &tools.ChatPatrolResult{
			Status:      "ok",
			Action:      params.Action,
			Message:     fmt.Sprintf("Cancelled %d schedule(s) in this conversation.", len(active)),
			AutoApprove: conv.AutoApprove,
			Schedules:   []tools.ChatPatrolScheduleView{},
		}, nil
	}
	targetID, err := s.resolveChatScheduleTargetID(ctx, user, conv.ID, params.ScheduleID)
	if err != nil {
		return nil, err
	}
	sched, err := s.store.CancelChatSchedule(ctx, user, conv.ID, targetID)
	if err != nil {
		return nil, err
	}
	s.emitPatrolUpdate(ctx, user, conv.ID, buf, conv.AutoApprove)
	list, _ := s.store.ListChatSchedules(ctx, user, conv.ID, false)
	view := toChatPatrolScheduleView(*sched)
	return &tools.ChatPatrolResult{
		Status:      "ok",
		Action:      params.Action,
		Message:     fmt.Sprintf("Cancelled schedule %q (id=%s).", view.Title, view.ID),
		AutoApprove: conv.AutoApprove,
		Schedule:    &view,
		Schedules:   toChatPatrolScheduleViews(list),
	}, nil
}

func (s *Server) handleSubresourceWithArg(w http.ResponseWriter, r *http.Request, user, convID, sub, subArg string) bool {
	switch sub {
	case "outputs":
		s.handleConversationOutputs(w, r, user, convID, subArg)
		return true
	case "team-files":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.handleTeamFile(w, r, user, convID, subArg)
			return true
		}
		return false
	case "audit":
		if r.Method == http.MethodGet {
			s.handleConversationAudit(w, r, convID)
			return true
		}
		return false
	case "queue":
		s.handleQueueRoutes(w, r, user, convID, subArg)
		return true
	case "schedules":
		s.handleScheduleRoutes(w, r, user, convID, subArg)
		return true
	default:
		return false
	}
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrChatScheduleNotFound), errors.Is(err, store.ErrConversationNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrInvalidChatSchedule):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleScheduleRoutes serves the per-conversation patrol/timer API under
// /conversations/{id}/schedules:
//   - GET /conversations/{id}/schedules
//   - POST /conversations/{id}/schedules
//   - PATCH /conversations/{id}/schedules/{scheduleID}
//   - DELETE /conversations/{id}/schedules/{scheduleID}
//   - POST /conversations/{id}/schedules/{scheduleID}/run-now
func (s *Server) handleScheduleRoutes(w http.ResponseWriter, r *http.Request, user, convID, subArg string) {
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	switch {
	case subArg == "" && r.Method == http.MethodGet:
		s.handleScheduleList(w, r, user, conv)
	case subArg == "" && r.Method == http.MethodPost:
		s.handleScheduleCreate(w, r, user, conv)
	case subArg != "" && !strings.Contains(subArg, "/") && r.Method == http.MethodPatch:
		s.handleSchedulePatch(w, r, user, conv, subArg)
	case subArg != "" && !strings.Contains(subArg, "/") && r.Method == http.MethodDelete:
		s.handleScheduleDelete(w, r, user, conv, subArg)
	case strings.HasSuffix(subArg, "/run-now") && r.Method == http.MethodPost:
		s.handleScheduleRunNow(w, r, user, conv, subArg)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleScheduleList(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation) {
	includeTerminal := r.URL.Query().Get("all") == "1"
	items, err := s.store.ListChatSchedules(r.Context(), user, conv.ID, includeTerminal)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"schedules":    items,
		"auto_approve": conv.AutoApprove,
	})
}

func (s *Server) handleScheduleCreate(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation) {
	var req struct {
		Title           string `json:"title"`
		Prompt          string `json:"prompt"`
		Cron            string `json:"cron"`
		Timezone        string `json:"timezone"`
		DurationSeconds int    `json:"duration_seconds"`
		MaxRuns         int    `json:"max_runs"`
		AutoApprove     *bool  `json:"auto_approve"`
		AllowMultiple   bool   `json:"allow_multiple"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	sched, err := s.store.CreateChatSchedule(r.Context(), store.ChatScheduleInput{
		ConversationID:  conv.ID,
		UserEmail:       user,
		Title:           req.Title,
		Prompt:          req.Prompt,
		CronExpr:        req.Cron,
		Timezone:        req.Timezone,
		DurationSeconds: req.DurationSeconds,
		MaxRuns:         req.MaxRuns,
		AllowMultiple:   req.AllowMultiple,
	})
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	if req.AutoApprove != nil {
		if err := s.store.SetAutoApprove(r.Context(), user, conv.ID, *req.AutoApprove); err != nil {
			writeScheduleError(w, err)
			return
		}
		conv.AutoApprove = *req.AutoApprove
	} else if !conv.AutoApprove {
		if err := s.store.SetAutoApprove(r.Context(), user, conv.ID, true); err == nil {
			conv.AutoApprove = true
		}
	}
	s.armShortScheduleTimer(sched.NextRunAt)
	s.emitPatrolUpdate(r.Context(), user, conv.ID, nil, conv.AutoApprove)
	items, _ := s.store.ListChatSchedules(r.Context(), user, conv.ID, false)
	writeJSON(w, map[string]any{
		"schedule":     sched,
		"schedules":    items,
		"auto_approve": conv.AutoApprove,
	})
}

func (s *Server) handleSchedulePatch(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, scheduleID string) {
	var req struct {
		Action          string  `json:"action"`
		Title           *string `json:"title"`
		Prompt          *string `json:"prompt"`
		Cron            *string `json:"cron"`
		Timezone        *string `json:"timezone"`
		DurationSeconds *int    `json:"duration_seconds"`
		MaxRuns         *int    `json:"max_runs"`
		Status          *string `json:"status"`
		AutoApprove     *bool   `json:"auto_approve"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	patch := store.ChatSchedulePatch{
		Title:           req.Title,
		Prompt:          req.Prompt,
		CronExpr:        req.Cron,
		Timezone:        req.Timezone,
		DurationSeconds: req.DurationSeconds,
		MaxRuns:         req.MaxRuns,
		Status:          req.Status,
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "pause":
		st := store.ChatScheduleStatusPaused
		patch.Status = &st
	case "resume":
		st := store.ChatScheduleStatusActive
		patch.Status = &st
	case "cancel", "stop":
		st := store.ChatScheduleStatusCancelled
		patch.Status = &st
	}
	sched, err := s.store.UpdateChatSchedule(r.Context(), user, conv.ID, scheduleID, patch)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	if req.AutoApprove != nil {
		if err := s.store.SetAutoApprove(r.Context(), user, conv.ID, *req.AutoApprove); err != nil {
			writeScheduleError(w, err)
			return
		}
		conv.AutoApprove = *req.AutoApprove
	}
	if sched.Status == store.ChatScheduleStatusActive {
		s.armShortScheduleTimer(sched.NextRunAt)
	}
	s.emitPatrolUpdate(r.Context(), user, conv.ID, nil, conv.AutoApprove)
	items, _ := s.store.ListChatSchedules(r.Context(), user, conv.ID, false)
	writeJSON(w, map[string]any{
		"schedule":     sched,
		"schedules":    items,
		"auto_approve": conv.AutoApprove,
	})
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, scheduleID string) {
	sched, err := s.store.CancelChatSchedule(r.Context(), user, conv.ID, scheduleID)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	s.emitPatrolUpdate(r.Context(), user, conv.ID, nil, conv.AutoApprove)
	items, _ := s.store.ListChatSchedules(r.Context(), user, conv.ID, false)
	writeJSON(w, map[string]any{
		"schedule":     sched,
		"schedules":    items,
		"auto_approve": conv.AutoApprove,
	})
}

func (s *Server) handleScheduleRunNow(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, subArg string) {
	scheduleID := strings.TrimSuffix(subArg, "/run-now")
	if scheduleID == "" || strings.Contains(scheduleID, "/") {
		http.Error(w, "invalid schedule id", http.StatusBadRequest)
		return
	}
	claimed, err := s.store.TriggerChatScheduleNow(r.Context(), user, conv.ID, scheduleID, maxPendingInputs)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	s.emitQueueUpdate(r.Context(), user, conv.ID)
	s.emitPatrolUpdate(r.Context(), user, conv.ID, nil, conv.AutoApprove)
	s.maybeDrainQueue(conv.ID)
	items, _ := s.store.ListChatSchedules(r.Context(), user, conv.ID, false)
	writeJSON(w, map[string]any{
		"schedule":     claimed.Schedule,
		"input_id":     claimed.Input.ID,
		"schedules":    items,
		"auto_approve": conv.AutoApprove,
	})
}
