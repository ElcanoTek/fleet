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
		//nolint:gosec // G706: all values are %q-quoted or integer server-generated fields.
		log.Printf("audit: chat schedule %q (run #%d) queued for conversation %q as input %q",
			item.Schedule.ID, item.Schedule.RunCount, item.Schedule.ConversationID, item.Input.ID)
		s.emitQueueUpdate(ctx, item.Input.UserEmail, item.Schedule.ConversationID)
		s.maybeDrainQueue(item.Schedule.ConversationID)
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
		if params.AutoApprove != nil {
			if err := s.store.SetAutoApprove(ctx, user, conv.ID, *params.AutoApprove); err != nil {
				return nil, fmt.Errorf("failed to update conversation auto_approve: %w", err)
			}
			conv.AutoApprove = *params.AutoApprove
		}

		resolveTargetID := func(explicit string) (string, error) {
			id := strings.TrimSpace(explicit)
			if id != "" {
				return id, nil
			}
			active, err := s.store.ListChatSchedules(ctx, user, conv.ID, false)
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

		switch params.Action {
		case tools.ChatPatrolActionList:
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

		case tools.ChatPatrolActionCreate:
			if params.AutoApprove == nil && !conv.AutoApprove {
				if err := s.store.SetAutoApprove(ctx, user, conv.ID, true); err == nil {
					conv.AutoApprove = true
				}
			}
			sched, err := s.store.CreateChatSchedule(ctx, store.ChatScheduleInput{
				ConversationID:  conv.ID,
				UserEmail:       user,
				Title:           params.Title,
				Prompt:          params.Prompt,
				CronExpr:        params.Cron,
				Timezone:        params.Timezone,
				DurationSeconds: params.DurationSeconds,
				MaxRuns:         params.MaxRuns,
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
				msg += " Tip: pass auto_approve=true or enable ⚡ Auto-approve in the composer so unattended patrol runs do not pause on tool approval cards."
			}
			return &tools.ChatPatrolResult{
				Status:      "ok",
				Action:      params.Action,
				Message:     msg,
				AutoApprove: conv.AutoApprove,
				Schedule:    &view,
				Schedules:   toChatPatrolScheduleViews(list),
			}, nil

		case tools.ChatPatrolActionUpdate:
			targetID, err := resolveTargetID(params.ScheduleID)
			if err != nil {
				return nil, err
			}
			var patch store.ChatSchedulePatch
			if strings.TrimSpace(params.Title) != "" {
				t := strings.TrimSpace(params.Title)
				patch.Title = &t
			}
			if strings.TrimSpace(params.Prompt) != "" {
				p := strings.TrimSpace(params.Prompt)
				patch.Prompt = &p
			}
			if strings.TrimSpace(params.Cron) != "" {
				c := strings.TrimSpace(params.Cron)
				patch.CronExpr = &c
			}
			if strings.TrimSpace(params.Timezone) != "" {
				tz := strings.TrimSpace(params.Timezone)
				patch.Timezone = &tz
			}
			if params.DurationSeconds > 0 {
				d := params.DurationSeconds
				patch.DurationSeconds = &d
			}
			if params.MaxRuns > 0 {
				m := params.MaxRuns
				patch.MaxRuns = &m
			}
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

		case tools.ChatPatrolActionPause, tools.ChatPatrolActionResume:
			targetID, err := resolveTargetID(params.ScheduleID)
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

		case tools.ChatPatrolActionCancel:
			if strings.EqualFold(strings.TrimSpace(params.ScheduleID), "all") {
				active, err := s.store.ListChatSchedules(ctx, user, conv.ID, false)
				if err != nil {
					return nil, err
				}
				for _, item := range active {
					_, _ = s.store.CancelChatSchedule(ctx, user, conv.ID, item.ID)
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
			targetID, err := resolveTargetID(params.ScheduleID)
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

		default:
			return nil, fmt.Errorf("unsupported action %q", params.Action)
		}
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

	writeScheduleFailure := func(err error) {
		switch {
		case errors.Is(err, store.ErrChatScheduleNotFound), errors.Is(err, store.ErrConversationNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, store.ErrInvalidChatSchedule):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	switch {
	case subArg == "" && r.Method == http.MethodGet:
		includeTerminal := r.URL.Query().Get("all") == "1"
		items, err := s.store.ListChatSchedules(r.Context(), user, convID, includeTerminal)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"schedules":    items,
			"auto_approve": conv.AutoApprove,
		})

	case subArg == "" && r.Method == http.MethodPost:
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
		if req.AutoApprove != nil {
			if err := s.store.SetAutoApprove(r.Context(), user, convID, *req.AutoApprove); err != nil {
				writeScheduleFailure(err)
				return
			}
			conv.AutoApprove = *req.AutoApprove
		} else if !conv.AutoApprove {
			if err := s.store.SetAutoApprove(r.Context(), user, convID, true); err == nil {
				conv.AutoApprove = true
			}
		}
		sched, err := s.store.CreateChatSchedule(r.Context(), store.ChatScheduleInput{
			ConversationID:  convID,
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
			writeScheduleFailure(err)
			return
		}
		s.armShortScheduleTimer(sched.NextRunAt)
		s.emitPatrolUpdate(r.Context(), user, convID, nil, conv.AutoApprove)
		items, _ := s.store.ListChatSchedules(r.Context(), user, convID, false)
		writeJSON(w, map[string]any{
			"schedule":     sched,
			"schedules":    items,
			"auto_approve": conv.AutoApprove,
		})

	case subArg != "" && !strings.Contains(subArg, "/") && r.Method == http.MethodPatch:
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
		if req.AutoApprove != nil {
			if err := s.store.SetAutoApprove(r.Context(), user, convID, *req.AutoApprove); err != nil {
				writeScheduleFailure(err)
				return
			}
			conv.AutoApprove = *req.AutoApprove
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
		sched, err := s.store.UpdateChatSchedule(r.Context(), user, convID, subArg, patch)
		if err != nil {
			writeScheduleFailure(err)
			return
		}
		if sched.Status == store.ChatScheduleStatusActive {
			s.armShortScheduleTimer(sched.NextRunAt)
		}
		s.emitPatrolUpdate(r.Context(), user, convID, nil, conv.AutoApprove)
		items, _ := s.store.ListChatSchedules(r.Context(), user, convID, false)
		writeJSON(w, map[string]any{
			"schedule":     sched,
			"schedules":    items,
			"auto_approve": conv.AutoApprove,
		})

	case subArg != "" && !strings.Contains(subArg, "/") && r.Method == http.MethodDelete:
		sched, err := s.store.CancelChatSchedule(r.Context(), user, convID, subArg)
		if err != nil {
			writeScheduleFailure(err)
			return
		}
		s.emitPatrolUpdate(r.Context(), user, convID, nil, conv.AutoApprove)
		items, _ := s.store.ListChatSchedules(r.Context(), user, convID, false)
		writeJSON(w, map[string]any{
			"schedule":     sched,
			"schedules":    items,
			"auto_approve": conv.AutoApprove,
		})

	case strings.HasSuffix(subArg, "/run-now") && r.Method == http.MethodPost:
		scheduleID := strings.TrimSuffix(subArg, "/run-now")
		if scheduleID == "" || strings.Contains(scheduleID, "/") {
			http.Error(w, "invalid schedule id", http.StatusBadRequest)
			return
		}
		claimed, err := s.store.TriggerChatScheduleNow(r.Context(), user, convID, scheduleID, maxPendingInputs)
		if err != nil {
			writeScheduleFailure(err)
			return
		}
		s.emitQueueUpdate(r.Context(), user, convID)
		s.emitPatrolUpdate(r.Context(), user, convID, nil, conv.AutoApprove)
		s.maybeDrainQueue(convID)
		items, _ := s.store.ListChatSchedules(r.Context(), user, convID, false)
		writeJSON(w, map[string]any{
			"schedule":     claimed.Schedule,
			"input_id":     claimed.Input.ID,
			"schedules":    items,
			"auto_approve": conv.AutoApprove,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
