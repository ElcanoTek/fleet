// Admin-managed workspace feature settings (internal/settings): the Features
// panel of the web admin page. GET lists every registered setting with its
// effective value + provenance; PUT/DELETE set or reset one override, applied
// LIVE through the injected service (no restart).
//
// Everything here is admin-gated (adminMiddleware, same as /admin/llm-providers)
// and secret-free by construction: the registry holds feature toggles and
// numeric bounds only — secret-bearing config (SMTP, webhook signing) is
// deliberately not admin-settable and stays in the host env file.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/settings"
)

type adminSettingWriteResponse struct {
	settings.Resolved
	Rollout      *ModelRolloutResponse `json:"rollout,omitempty"`
	RolloutError string                `json:"rollout_error,omitempty"`
	RolloutRetry *ModelRolloutRequest  `json:"rollout_retry,omitempty"`
}

// workspaceSettingsService is the seam the admin settings endpoints call,
// satisfied by *settings.Service and injected via WithWorkspaceSettings.
type workspaceSettingsService interface {
	Snapshot(ctx context.Context) ([]settings.Resolved, error)
	Set(ctx context.Context, key, value, updatedBy string) (settings.Resolved, error)
	Reset(ctx context.Context, key, updatedBy string) (settings.Resolved, error)
}

// WithWorkspaceSettings injects the admin feature-settings service. Omitted
// (tests, mock mode), the /admin/settings endpoints answer 501 so the panel
// reports "unavailable" instead of silently persisting to nowhere.
func WithWorkspaceSettings(svc workspaceSettingsService) Option {
	return func(s *Server) { s.workspaceSettings = svc }
}

// handleAdminSettings serves GET /admin/settings — the full resolved registry.
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.workspaceSettings == nil {
		http.Error(w, "workspace settings unavailable", http.StatusNotImplemented)
		return
	}
	resolved, err := s.workspaceSettings.Snapshot(r.Context())
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, r, map[string]any{"settings": resolved})
}

// handleAdminSettingItem serves /admin/settings/{key}: PUT sets an override,
// DELETE resets it to the env-derived default. Both respond with the setting's
// new resolved state so the panel can re-render the row without a refetch.
func (s *Server) handleAdminSettingItem(w http.ResponseWriter, r *http.Request) {
	if s.workspaceSettings == nil {
		http.Error(w, "workspace settings unavailable", http.StatusNotImplemented)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/admin/settings/")
	if key == "" || strings.Contains(key, "/") {
		http.Error(w, "setting key required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Value       string `json:"value"`
			Rollout     bool   `json:"rollout"`
			NotifyChats *bool  `json:"notify_chats"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		s.writeAdminSettingItem(w, r, key, body.Value, body.Rollout, body.NotifyChats)
	case http.MethodDelete:
		rollout := r.URL.Query().Get("rollout") == "true"
		var notifyChats *bool
		if raw := r.URL.Query().Get("notify_chats"); raw == "true" || raw == "false" {
			v := raw == "true"
			notifyChats = &v
		}
		s.resetAdminSettingItem(w, r, key, rollout, notifyChats)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeAdminSettingItem(w http.ResponseWriter, r *http.Request, key, value string, wantRollout bool, notifyChats *bool) {
	ctx := r.Context()
	if isModelTierSettingKey(key) {
		release, err := s.acquireModelRolloutLock(ctx)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		defer release()
	}
	prevFallback := agentcore.CurrentFallbackModel()
	prevTask := agentcore.CurrentTaskModel()
	if key == "advanced_model" && wantRollout {
		if normVal, verr := validateRolloutModelSlug(key, value); verr == nil {
			if perr := s.preparePendingFallbackBeforeTierWriteLocked(ctx, prevFallback, normVal); perr != nil {
				writeInternalError(w, r, perr)
				return
			}
		}
	}
	resolved, err := s.workspaceSettings.Set(ctx, key, value, userFromCtx(ctx))
	if err != nil {
		httpErrorForSetting(w, r, err)
		return
	}
	if !wantRollout {
		if key == "advanced_model" {
			if cerr := s.clearPendingFallbackFromLocked(ctx); cerr != nil {
				writeInternalError(w, r, cerr)
				return
			}
		}
		writeJSON(w, r, resolved)
		return
	}
	rollout, retryReq, rerr := s.rolloutAfterSettingChangeLocked(ctx, key, prevFallback, prevTask, notifyChats)
	resp := adminSettingWriteResponse{Resolved: resolved, Rollout: rollout}
	if rerr != nil {
		resp.RolloutError = rolloutErrorText(ctx, r, rerr)
		resp.RolloutRetry = retryReq
	}
	writeJSON(w, r, resp)
}

func (s *Server) resetAdminSettingItem(w http.ResponseWriter, r *http.Request, key string, wantRollout bool, notifyChats *bool) {
	ctx := r.Context()
	if isModelTierSettingKey(key) {
		release, err := s.acquireModelRolloutLock(ctx)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		defer release()
	}
	prevFallback := agentcore.CurrentFallbackModel()
	prevTask := agentcore.CurrentTaskModel()
	if key == "advanced_model" && wantRollout {
		snap, serr := s.workspaceSettings.Snapshot(ctx)
		if serr != nil {
			writeInternalError(w, r, serr)
			return
		}
		var resetDefault string
		for _, item := range snap {
			if item.Key == "advanced_model" {
				resetDefault = item.Default
				break
			}
		}
		if perr := s.preparePendingFallbackBeforeTierWriteLocked(ctx, prevFallback, resetDefault); perr != nil {
			writeInternalError(w, r, perr)
			return
		}
	}
	resolved, err := s.workspaceSettings.Reset(ctx, key, userFromCtx(ctx))
	if err != nil {
		httpErrorForSetting(w, r, err)
		return
	}
	if !wantRollout {
		if key == "advanced_model" {
			if cerr := s.clearPendingFallbackFromLocked(ctx); cerr != nil {
				writeInternalError(w, r, cerr)
				return
			}
		}
		writeJSON(w, r, resolved)
		return
	}
	rollout, retryReq, rerr := s.rolloutAfterSettingChangeLocked(ctx, key, prevFallback, prevTask, notifyChats)
	resp := adminSettingWriteResponse{Resolved: resolved, Rollout: rollout}
	if rerr != nil {
		resp.RolloutError = rolloutErrorText(ctx, r, rerr)
		resp.RolloutRetry = retryReq
	}
	writeJSON(w, r, resp)
}

// httpErrorForSetting maps service errors: unknown key → 404, a validation
// failure (reported before anything is persisted) → 400, a value the apply
// hook refused for the current configuration (settings.ApplyError, undone
// already) → 409 with the hook's text, the same refusal whose undo write
// failed → 500 with the hook's text and what to do, and anything else (the
// store) → an internal error.
func httpErrorForSetting(w http.ResponseWriter, r *http.Request, err error) {
	var applyErr *settings.ApplyError
	switch {
	case errors.Is(err, settings.ErrUnknownKey):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, settings.ErrInvalidValue):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &applyErr) && applyErr.Compensation == "":
		http.Error(w, applyErr.Error(), http.StatusConflict)
	case errors.As(err, &applyErr):
		logInternalError(r.Context(), r, err, 1)
		http.Error(w, applyErr.Error(), http.StatusInternalServerError)
	default:
		writeInternalErrorFromHelper(w, r, err)
	}
}
