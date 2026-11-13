package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TaskModelRolloutFunc upgrades primary and/or fallback models across upcoming
// scheduled and pending tasks (satisfied by (*sched/storage.Storage).RolloutTaskModels).
type TaskModelRolloutFunc func(ctx context.Context, opts models.TaskModelRollout, dryRun bool) (models.TaskModelRolloutResult, error)

// WithTaskModelRollout wires the scheduler storage's task model rollout into
// the admin model-rollout and settings endpoints.
func WithTaskModelRollout(fn TaskModelRolloutFunc) Option {
	return func(s *Server) { s.taskModelRollout = fn }
}

// ModelRolloutRequest is the body of POST /admin/models/rollout.
type ModelRolloutRequest struct {
	// SyncCurrent, when true, upgrades all existing chats to CurrentDefaultModel
	// and all upcoming scheduled/pending tasks to CurrentTaskModel +
	// CurrentFallbackModel.
	SyncCurrent bool `json:"sync_current"`
	// Model, when non-empty, upgrades matching chats and task primary models.
	Model string `json:"model"`
	// TaskModel, when non-empty and Model is empty, upgrades only task primary
	// models without changing chat conversations.
	TaskModel string `json:"task_model"`
	// FromModel restricts chat and primary-task updates to items currently on
	// FromModel (empty updates all items whose model differs).
	FromModel string `json:"from_model"`
	// FallbackModel, when non-nil, updates fallback_model on upcoming tasks
	// ("" clears fallback_model to NULL).
	FallbackModel *string `json:"fallback_model"`
	// FromFallbackModel restricts fallback_model updates to tasks currently on
	// FromFallbackModel.
	FromFallbackModel string `json:"from_fallback_model"`
	// UpgradeFromFallback, when non-empty alongside a non-empty FallbackModel,
	// also upgrades chats and task primary models currently pinned to this
	// previous fallback slug onto the new FallbackModel.
	UpgradeFromFallback string `json:"upgrade_from_fallback"`
	// NotifyChats controls whether non-empty chats whose model changes receive
	// a UI-only model_upgraded notice entry. Defaults to true when omitted.
	NotifyChats *bool `json:"notify_chats"`
	// RecurringOnly restricts task updates to recurring cron tasks.
	RecurringOnly bool `json:"recurring_only"`
	// DryRun computes exact counts without mutating chats or tasks.
	DryRun bool `json:"dry_run"`
}

// ModelRolloutResponse summarizes a dry-run or executed model rollout across
// chats and scheduled/recurring tasks.
type ModelRolloutResponse struct {
	DryRun                bool   `json:"dry_run"`
	PrimaryModel          string `json:"primary_model,omitempty"`
	TaskModel             string `json:"task_model,omitempty"`
	FallbackModel         string `json:"fallback_model,omitempty"`
	LockdownIncluded      bool   `json:"lockdown_included"`
	ChatsMatched          int    `json:"chats_matched"`
	ChatsUpdated          int    `json:"chats_updated"`
	ChatsNotified         int    `json:"chats_notified"`
	TasksScanned          int    `json:"tasks_scanned"`
	TasksUpdated          int    `json:"tasks_updated"`
	TasksPrimaryUpdated   int    `json:"tasks_primary_updated"`
	TasksFallbackUpdated  int    `json:"tasks_fallback_updated"`
	TasksRecurringUpdated int    `json:"tasks_recurring_updated"`
}

func validateRolloutModelSlug(field, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > 200 || strings.ContainsAny(v, " \t\n") {
		return "", fmt.Errorf("%s: want a model slug like provider/model, got %q", field, raw)
	}
	if i := strings.Index(v, "/"); i <= 0 || i == len(v)-1 {
		return "", fmt.Errorf("%s: want a model slug like provider/model, got %q", field, raw)
	}
	return v, nil
}

// handleAdminModelRollout serves POST /admin/models/rollout.
func (s *Server) handleAdminModelRollout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		http.Error(w, "chat store unavailable", http.StatusNotImplemented)
		return
	}
	var req ModelRolloutRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	res, status, err := s.executeModelRollout(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, r, res)
}

func (s *Server) executeModelRollout(ctx context.Context, req ModelRolloutRequest) (ModelRolloutResponse, int, error) {
	notifyChats := true
	if req.NotifyChats != nil {
		notifyChats = *req.NotifyChats
	}

	var (
		chatTarget        string
		chatFrom          = strings.TrimSpace(req.FromModel)
		taskPrimaryTarget *string
		taskPrimaryFrom   = strings.TrimSpace(req.FromModel)
		taskFallback      *string
		taskFallbackFrom  = strings.TrimSpace(req.FromFallbackModel)
	)

	if req.SyncCurrent {
		def := agentcore.CurrentDefaultModel()
		tm := agentcore.CurrentTaskModel()
		fb := agentcore.CurrentFallbackModel()
		chatTarget = def
		taskPrimaryTarget = &tm
		taskFallback = &fb
	} else {
		if strings.TrimSpace(req.Model) != "" {
			v, err := validateRolloutModelSlug("model", req.Model)
			if err != nil {
				return ModelRolloutResponse{}, http.StatusBadRequest, err
			}
			chatTarget = v
			taskPrimaryTarget = &v
		} else if strings.TrimSpace(req.TaskModel) != "" {
			v, err := validateRolloutModelSlug("task_model", req.TaskModel)
			if err != nil {
				return ModelRolloutResponse{}, http.StatusBadRequest, err
			}
			taskPrimaryTarget = &v
		}

		if req.FallbackModel != nil {
			trimmed := strings.TrimSpace(*req.FallbackModel)
			if trimmed == "" {
				empty := ""
				taskFallback = &empty
			} else {
				v, err := validateRolloutModelSlug("fallback_model", trimmed)
				if err != nil {
					return ModelRolloutResponse{}, http.StatusBadRequest, err
				}
				taskFallback = &v
				if upFrom := strings.TrimSpace(req.UpgradeFromFallback); upFrom != "" && upFrom != v && chatTarget == "" {
					chatTarget = v
					chatFrom = upFrom
					if taskPrimaryTarget == nil {
						taskPrimaryTarget = &v
						taskPrimaryFrom = upFrom
					}
				}
			}
		}
	}

	if chatTarget == "" && taskPrimaryTarget == nil && taskFallback == nil {
		return ModelRolloutResponse{}, http.StatusBadRequest, fmt.Errorf("specify model, task_model, fallback_model, or sync_current=true")
	}

	out := ModelRolloutResponse{
		DryRun:       req.DryRun,
		PrimaryModel: chatTarget,
	}
	if taskPrimaryTarget != nil {
		out.TaskModel = *taskPrimaryTarget
	}
	if taskFallback != nil {
		out.FallbackModel = *taskFallback
	}

	if chatTarget != "" && s.store != nil {
		includeLockdown := true
		if s.cfg != nil {
			if allow := s.cfg.LockdownModels(); len(allow) > 0 {
				includeLockdown = config.ModelAllowed(chatTarget, allow)
			}
		}
		out.LockdownIncluded = includeLockdown
		if req.DryRun {
			convs, err := s.store.ListConversationsForModelUpdate(ctx, chatFrom, includeLockdown)
			if err != nil {
				return ModelRolloutResponse{}, http.StatusInternalServerError, fmt.Errorf("list conversations: %w", err)
			}
			out.ChatsMatched = len(convs)
			for _, c := range convs {
				if strings.TrimSpace(c.Model) != chatTarget {
					out.ChatsUpdated++
					if notifyChats && c.HasMessages {
						out.ChatsNotified++
					}
				}
			}
		} else {
			chatRes, err := s.store.RolloutConversationModel(ctx, chatTarget, chatFrom, includeLockdown, notifyChats)
			if err != nil {
				return ModelRolloutResponse{}, http.StatusInternalServerError, fmt.Errorf("rollout conversations: %w", err)
			}
			out.ChatsMatched = chatRes.MatchedCount
			out.ChatsUpdated = chatRes.UpdatedCount
			out.ChatsNotified = chatRes.NotifiedCount
		}
	}

	if (taskPrimaryTarget != nil || taskFallback != nil) && s.taskModelRollout != nil {
		taskRes, err := s.taskModelRollout(ctx, models.TaskModelRollout{
			Model:             taskPrimaryTarget,
			FromModel:         taskPrimaryFrom,
			FallbackModel:     taskFallback,
			FromFallbackModel: taskFallbackFrom,
			RecurringOnly:     req.RecurringOnly,
		}, req.DryRun)
		if err != nil {
			return ModelRolloutResponse{}, http.StatusInternalServerError, fmt.Errorf("rollout tasks: %w", err)
		}
		out.TasksScanned = taskRes.ScannedCount
		out.TasksUpdated = taskRes.UpdatedCount
		out.TasksPrimaryUpdated = taskRes.PrimaryUpdatedCount
		out.TasksFallbackUpdated = taskRes.FallbackUpdatedCount
		out.TasksRecurringUpdated = taskRes.RecurringUpdatedCount
	}

	if !req.DryRun {
		logging.From(ctx).Info("Model rollout completed",
			"actor", safe.LogSafe(userFromCtx(ctx)),
			"primary_model", safe.LogSafe(out.PrimaryModel),
			"task_model", safe.LogSafe(out.TaskModel),
			"fallback_model", safe.LogSafe(out.FallbackModel),
			"chats_updated", out.ChatsUpdated,
			"chats_notified", out.ChatsNotified,
			"tasks_updated", out.TasksUpdated,
		)
	}
	return out, http.StatusOK, nil
}

// rolloutAfterSettingChange builds and executes the appropriate rollout when an
// admin updates default_model, advanced_model, or task_model with rollout=true.
func (s *Server) rolloutAfterSettingChange(ctx context.Context, key, prevFallback, prevTask string, notifyChats *bool) (*ModelRolloutResponse, error) {
	var req ModelRolloutRequest
	req.NotifyChats = notifyChats

	switch key {
	case "default_model":
		nextDefault := agentcore.CurrentDefaultModel()
		nextTask := agentcore.CurrentTaskModel()
		// Upgrade all chats to the new primary model, and upgrade task primary
		// models too whenever task_model follows default_model (or matches it).
		if nextTask == nextDefault {
			req.Model = nextDefault
		} else {
			// A dedicated task_model override is in place: roll out chats via
			// executeModelRollout without clobbering the custom task_model.
			return s.rolloutChatsOnly(ctx, nextDefault, notifyChats)
		}
	case "advanced_model":
		nextFallback := agentcore.CurrentFallbackModel()
		req.FallbackModel = &nextFallback
		// Also upgrade chats/tasks whose primary model was explicitly pinned to
		// the previous fallback model (unless the previous fallback happened to
		// be the current primary default model).
		curDefault := agentcore.CurrentDefaultModel()
		if prevFallback != "" && prevFallback != nextFallback && prevFallback != curDefault {
			req.UpgradeFromFallback = prevFallback
		}
	case "task_model":
		nextTask := agentcore.CurrentTaskModel()
		if nextTask == "" && prevTask == "" {
			return nil, nil
		}
		req.TaskModel = nextTask
	default:
		return nil, nil
	}

	res, _, err := s.executeModelRollout(ctx, req)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *Server) rolloutChatsOnly(ctx context.Context, target string, notifyChats *bool) (*ModelRolloutResponse, error) {
	if s.store == nil || strings.TrimSpace(target) == "" {
		return nil, nil
	}
	notify := true
	if notifyChats != nil {
		notify = *notifyChats
	}
	includeLockdown := true
	if s.cfg != nil {
		if allow := s.cfg.LockdownModels(); len(allow) > 0 {
			includeLockdown = config.ModelAllowed(target, allow)
		}
	}
	chatRes, err := s.store.RolloutConversationModel(ctx, target, "", includeLockdown, notify)
	if err != nil {
		return nil, err
	}
	return &ModelRolloutResponse{
		PrimaryModel:     target,
		LockdownIncluded: includeLockdown,
		ChatsMatched:     chatRes.MatchedCount,
		ChatsUpdated:     chatRes.UpdatedCount,
		ChatsNotified:    chatRes.NotifiedCount,
	}, nil
}
