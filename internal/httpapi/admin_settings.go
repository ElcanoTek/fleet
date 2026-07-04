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
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/settings"
)

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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"settings": resolved})
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
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		resolved, err := s.workspaceSettings.Set(r.Context(), key, body.Value, userFromCtx(r.Context()))
		if err != nil {
			httpErrorForSetting(w, err)
			return
		}
		writeJSON(w, resolved)
	case http.MethodDelete:
		resolved, err := s.workspaceSettings.Reset(r.Context(), key, userFromCtx(r.Context()))
		if err != nil {
			httpErrorForSetting(w, err)
			return
		}
		writeJSON(w, resolved)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// httpErrorForSetting maps service errors: unknown key → 404, a validation
// failure (reported before anything is persisted) → 400, persist/apply → 500.
func httpErrorForSetting(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, settings.ErrUnknownKey):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, settings.ErrInvalidValue):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
