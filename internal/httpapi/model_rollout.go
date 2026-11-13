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

type modelRolloutAdvisoryLocker interface {
	AcquireModelRolloutLock(ctx context.Context) (func(), error)
}

func isModelTierSettingKey(key string) bool {
	return key == "default_model" || key == "advanced_model" || key == "task_model"
}

func (s *Server) configuredTaskFallbackModel() string {
	if s.cfg == nil {
		return ""
	}
	return strings.TrimSpace(s.cfg.TaskFallbackModel)
}

func (s *Server) acquireModelRolloutLock(ctx context.Context) (func(), error) {
	s.modelRolloutMu.Lock()
	if locker, ok := s.store.(modelRolloutAdvisoryLocker); ok {
		releaseDB, err := locker.AcquireModelRolloutLock(ctx)
		if err != nil {
			s.modelRolloutMu.Unlock()
			return nil, err
		}
		return func() {
			releaseDB()
			s.modelRolloutMu.Unlock()
		}, nil
	}
	return func() {
		s.modelRolloutMu.Unlock()
	}, nil
}

// WithTaskModelRollout wires the scheduler storage's task model rollout into
// the admin model-rollout and settings endpoints.
func WithTaskModelRollout(fn TaskModelRolloutFunc) Option {
	return func(s *Server) { s.taskModelRollout = fn }
}

// ModelRolloutRequest is the body of POST /admin/models/rollout.
type ModelRolloutRequest struct {
	// SyncCurrent, when true, upgrades all existing chats to CurrentDefaultModel
	// and all upcoming scheduled/pending tasks to CurrentTaskModel +
	// the effective task fallback (FLEET_TASK_FALLBACK_MODEL if set, else
	// CurrentFallbackModel).
	SyncCurrent bool `json:"sync_current"`
	// Model, when non-empty, upgrades matching chats and task primary models.
	Model string `json:"model"`
	// ChatModel, when non-empty and Model is empty, upgrades only chat
	// conversations without changing scheduled tasks.
	ChatModel string `json:"chat_model,omitempty"`
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
	// OnlySetFallbacks, when true alongside FallbackModel, updates only tasks
	// that already have a non-empty fallback_model (preserving NULL on bare API
	// tasks).
	OnlySetFallbacks bool `json:"only_set_fallbacks,omitempty"`
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

type resolvedRolloutTargets struct {
	chatTarget        string
	chatFrom          string
	taskPrimaryTarget *string
	taskPrimaryFrom   string
	taskFallback      *string
	taskFallbackFrom  string
	onlySetFallbacks  bool
	notifyChats       bool
}

func (s *Server) resolveRolloutTargets(req ModelRolloutRequest) (resolvedRolloutTargets, error) {
	t := resolvedRolloutTargets{
		chatFrom:         strings.TrimSpace(req.FromModel),
		taskPrimaryFrom:  strings.TrimSpace(req.FromModel),
		taskFallbackFrom: strings.TrimSpace(req.FromFallbackModel),
		onlySetFallbacks: req.OnlySetFallbacks,
		notifyChats:      req.NotifyChats == nil || *req.NotifyChats,
	}

	if req.SyncCurrent {
		def := agentcore.CurrentDefaultModel()
		tm := agentcore.CurrentTaskModel()
		fb := s.configuredTaskFallbackModel()
		if fb == "" {
			fb = agentcore.CurrentFallbackModel()
		}
		t.chatTarget = def
		t.taskPrimaryTarget = &tm
		t.taskFallback = &fb
		t.onlySetFallbacks = true
	} else {
		if err := s.resolveExplicitRolloutSlugs(req, &t); err != nil {
			return resolvedRolloutTargets{}, err
		}
	}

	if t.chatTarget == "" && t.taskPrimaryTarget == nil && t.taskFallback == nil {
		return resolvedRolloutTargets{}, fmt.Errorf("specify model, chat_model, task_model, fallback_model, or sync_current=true")
	}
	return t, nil
}

func (s *Server) resolveExplicitRolloutSlugs(req ModelRolloutRequest, t *resolvedRolloutTargets) error {
	if strings.TrimSpace(req.Model) != "" {
		v, err := validateRolloutModelSlug("model", req.Model)
		if err != nil {
			return err
		}
		t.chatTarget = v
		t.taskPrimaryTarget = &v
	} else {
		if strings.TrimSpace(req.ChatModel) != "" {
			v, err := validateRolloutModelSlug("chat_model", req.ChatModel)
			if err != nil {
				return err
			}
			t.chatTarget = v
		}
		if strings.TrimSpace(req.TaskModel) != "" {
			v, err := validateRolloutModelSlug("task_model", req.TaskModel)
			if err != nil {
				return err
			}
			t.taskPrimaryTarget = &v
		}
	}

	if req.FallbackModel != nil {
		trimmed := strings.TrimSpace(*req.FallbackModel)
		if trimmed == "" {
			empty := ""
			t.taskFallback = &empty
		} else {
			v, err := validateRolloutModelSlug("fallback_model", trimmed)
			if err != nil {
				return err
			}
			t.taskFallback = &v
			if upFrom := strings.TrimSpace(req.UpgradeFromFallback); upFrom != "" && upFrom != v && t.chatTarget == "" {
				t.chatTarget = v
				t.chatFrom = upFrom
				if t.taskPrimaryTarget == nil && upFrom != agentcore.CurrentTaskModel() {
					t.taskPrimaryTarget = &v
					t.taskPrimaryFrom = upFrom
				}
			}
		}
	}
	return nil
}

func (s *Server) executeModelRollout(ctx context.Context, req ModelRolloutRequest) (ModelRolloutResponse, int, error) {
	if !req.DryRun {
		// Pre-validate explicit slugs before acquiring the cross-store lock so
		// invalid inputs fail fast with 400.
		if !req.SyncCurrent {
			if _, err := s.resolveRolloutTargets(req); err != nil {
				return ModelRolloutResponse{}, http.StatusBadRequest, err
			}
		}
		release, err := s.acquireModelRolloutLock(ctx)
		if err != nil {
			return ModelRolloutResponse{}, http.StatusInternalServerError, err
		}
		defer release()
	}
	return s.executeModelRolloutLocked(ctx, req)
}

func (s *Server) executeModelRolloutLocked(ctx context.Context, req ModelRolloutRequest) (ModelRolloutResponse, int, error) {
	t, err := s.resolveRolloutTargets(req)
	if err != nil {
		return ModelRolloutResponse{}, http.StatusBadRequest, err
	}

	out := ModelRolloutResponse{
		DryRun:       req.DryRun,
		PrimaryModel: t.chatTarget,
	}
	if t.taskPrimaryTarget != nil {
		out.TaskModel = *t.taskPrimaryTarget
	}
	if t.taskFallback != nil {
		out.FallbackModel = *t.taskFallback
	}

	if err := s.runChatRolloutPhase(ctx, t, req.DryRun, &out); err != nil {
		return ModelRolloutResponse{}, http.StatusInternalServerError, err
	}
	if err := s.runTaskRolloutPhase(ctx, t, req.RecurringOnly, req.DryRun, &out); err != nil {
		return ModelRolloutResponse{}, http.StatusInternalServerError, err
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

func (s *Server) runChatRolloutPhase(ctx context.Context, t resolvedRolloutTargets, dryRun bool, out *ModelRolloutResponse) error {
	if t.chatTarget == "" || s.store == nil {
		return nil
	}
	includeLockdown := true
	if s.cfg != nil {
		if allow := s.cfg.LockdownModels(); len(allow) > 0 {
			includeLockdown = config.ModelAllowed(t.chatTarget, allow)
		}
	}
	out.LockdownIncluded = includeLockdown
	if dryRun {
		convs, err := s.store.ListConversationsForModelUpdate(ctx, t.chatFrom, includeLockdown)
		if err != nil {
			return fmt.Errorf("list conversations: %w", err)
		}
		out.ChatsMatched = len(convs)
		for _, c := range convs {
			if strings.TrimSpace(c.Model) != t.chatTarget {
				out.ChatsUpdated++
				if t.notifyChats && c.HasMessages {
					out.ChatsNotified++
				}
			}
		}
		return nil
	}
	chatRes, err := s.store.RolloutConversationModel(ctx, t.chatTarget, t.chatFrom, includeLockdown, t.notifyChats)
	if err != nil {
		return fmt.Errorf("rollout conversations: %w", err)
	}
	out.ChatsMatched = chatRes.MatchedCount
	out.ChatsUpdated = chatRes.UpdatedCount
	out.ChatsNotified = chatRes.NotifiedCount
	return nil
}

func (s *Server) runTaskRolloutPhase(ctx context.Context, t resolvedRolloutTargets, recurringOnly, dryRun bool, out *ModelRolloutResponse) error {
	if (t.taskPrimaryTarget == nil && t.taskFallback == nil) || s.taskModelRollout == nil {
		return nil
	}
	taskRes, err := s.taskModelRollout(ctx, models.TaskModelRollout{
		Model:             t.taskPrimaryTarget,
		FromModel:         t.taskPrimaryFrom,
		FallbackModel:     t.taskFallback,
		FromFallbackModel: t.taskFallbackFrom,
		OnlySetFallbacks:  t.onlySetFallbacks,
		RecurringOnly:     recurringOnly,
	}, dryRun)
	if err != nil {
		return fmt.Errorf("rollout tasks: %w", err)
	}
	out.TasksScanned = taskRes.ScannedCount
	out.TasksUpdated = taskRes.UpdatedCount
	out.TasksPrimaryUpdated = taskRes.PrimaryUpdatedCount
	out.TasksFallbackUpdated = taskRes.FallbackUpdatedCount
	out.TasksRecurringUpdated = taskRes.RecurringUpdatedCount
	return nil
}

// rolloutAfterSettingChangeLocked builds and executes the appropriate rollout
// when an admin updates default_model, advanced_model, or task_model with
// rollout=true. Caller holds s.acquireModelRolloutLock.
func (s *Server) rolloutAfterSettingChangeLocked(ctx context.Context, key, prevFallback, prevTask string, notifyChats *bool) (*ModelRolloutResponse, error) {
	req := ModelRolloutRequest{NotifyChats: notifyChats}

	switch key {
	case "default_model":
		nextDefault := agentcore.CurrentDefaultModel()
		nextTask := agentcore.CurrentTaskModel()
		// Upgrade all chats to the new primary model, and upgrade task primary
		// models too whenever task_model follows default_model (or matches it).
		if nextTask == nextDefault {
			req.Model = nextDefault
		} else {
			req.ChatModel = nextDefault
		}
	case "advanced_model":
		nextFallback := agentcore.CurrentFallbackModel()
		curDefault := agentcore.CurrentDefaultModel()
		curTask := agentcore.CurrentTaskModel()
		if s.configuredTaskFallbackModel() == "" {
			// When FLEET_TASK_FALLBACK_MODEL is unset, task fallback follows
			// advanced_model: update existing tasks that have a fallback_model
			// configured while preserving NULL on bare API tasks.
			req.FallbackModel = &nextFallback
			req.OnlySetFallbacks = true
			if prevFallback != "" && prevFallback != nextFallback && prevFallback != curDefault {
				req.UpgradeFromFallback = prevFallback
			}
		} else {
			// Explicit FLEET_TASK_FALLBACK_MODEL owns task fallbacks, so do not
			// overwrite task fallback_model from advanced_model. Still upgrade
			// chats (and task primaries unless task_model matches prevFallback)
			// that were explicitly pinned to prevFallback.
			if prevFallback == "" || prevFallback == nextFallback || prevFallback == curDefault {
				return nil, nil
			}
			req.FromModel = prevFallback
			if prevFallback != curTask {
				req.Model = nextFallback
			} else {
				req.ChatModel = nextFallback
			}
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

	res, _, err := s.executeModelRolloutLocked(ctx, req)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
