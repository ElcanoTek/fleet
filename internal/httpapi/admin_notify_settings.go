// Admin-managed task notification settings (internal/notifyadmin): the
// Notifications panel of the web admin page. GET returns the effective config
// (admin row or env) with secrets as has_* booleans only; PUT saves the admin
// row and hot-swaps the live notifier; DELETE reverts to the env config;
// POST /test fires one real delivery attempt of a synthetic event.
//
// Security invariants (mirroring LLM providers / MCP credential accounts):
//   - Secret VALUES (SMTP password, webhook signing secret) are write-only.
//     No response ever carries one; the edit form's fields start empty
//     ("leave blank to keep").
//   - Everything is admin-gated (adminMiddleware).
//   - The test send decrypts host-side only and returns a key-free result.

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/notifyadmin"
	"github.com/ElcanoTek/fleet/internal/store"
)

// notifySettingsService is the seam the endpoints call, satisfied by
// *notifyadmin.Service and injected via WithNotifySettings.
type notifySettingsService interface {
	View(ctx context.Context) (notifyadmin.View, error)
	Save(ctx context.Context, in store.NotifySettingsInput, updatedBy string) (notifyadmin.View, error)
	Revert(ctx context.Context, updatedBy string) (notifyadmin.View, error)
	Test(ctx context.Context, channel string) (notify.TestResult, error)
}

// WithNotifySettings injects the admin notification-settings service. Omitted
// (tests, mock mode), the /admin/notify-settings endpoints answer 501.
func WithNotifySettings(svc notifySettingsService) Option {
	return func(s *Server) { s.notifySettings = svc }
}

// notifySettingsBody is the PUT payload. The secret fields follow the
// write-only convention: absent (nil) = keep the stored value, "" = clear.
type notifySettingsBody struct {
	NotifyOn            string  `json:"notify_on"`
	SMTPHost            string  `json:"smtp_host"`
	SMTPPort            string  `json:"smtp_port"`
	SMTPUsername        string  `json:"smtp_username"`
	SMTPPassword        *string `json:"smtp_password"`
	SMTPFrom            string  `json:"smtp_from"`
	EmailTo             string  `json:"email_to"`
	WebhookURL          string  `json:"webhook_url"`
	WebhookMethod       string  `json:"webhook_method"`
	WebhookBodyTemplate string  `json:"webhook_body_template"`
	WebhookSecret       *string `json:"webhook_secret"`
}

// handleAdminNotifySettings serves /admin/notify-settings: GET the effective
// view, PUT the admin row, DELETE to revert to env config.
func (s *Server) handleAdminNotifySettings(w http.ResponseWriter, r *http.Request) {
	if s.notifySettings == nil {
		http.Error(w, "notification settings unavailable", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := s.notifySettings.View(r.Context())
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, r, view)
	case http.MethodPut:
		var body notifySettingsBody
		if !decodeJSONBody(w, r, &body) {
			return
		}
		in := store.NotifySettingsInput{
			NotifyOn: body.NotifyOn, SMTPHost: body.SMTPHost, SMTPPort: body.SMTPPort,
			SMTPUsername: body.SMTPUsername, SMTPPassword: body.SMTPPassword,
			SMTPFrom: body.SMTPFrom, EmailTo: body.EmailTo,
			WebhookURL: body.WebhookURL, WebhookMethod: body.WebhookMethod,
			WebhookBodyTemplate: body.WebhookBodyTemplate, WebhookSecret: body.WebhookSecret,
		}
		view, err := s.notifySettings.Save(r.Context(), in, userFromCtx(r.Context()))
		if err != nil {
			httpErrorForNotifySettings(w, r, err)
			return
		}
		writeJSON(w, r, view)
	case http.MethodDelete:
		view, err := s.notifySettings.Revert(r.Context(), userFromCtx(r.Context()))
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		writeJSON(w, r, view)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminNotifySettingsTest serves POST /admin/notify-settings/test — one
// real delivery attempt over {"channel": "email"|"webhook"} using the
// effective config. The response is a key-free {ok, detail, latency_ms}.
func (s *Server) handleAdminNotifySettingsTest(w http.ResponseWriter, r *http.Request) {
	if s.notifySettings == nil {
		http.Error(w, "notification settings unavailable", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	result, err := s.notifySettings.Test(r.Context(), body.Channel)
	if err != nil {
		httpErrorForNotifySettings(w, r, err)
		return
	}
	writeJSON(w, r, result)
}

// errNotifySecretsUnreadable answers a save or test that needed the stored
// secrets after the encryption key changed: what to do, without the
// decryption error (logged).
var errNotifySecretsUnreadable = errors.New("stored notification secrets cannot be decrypted (was FLEET_MCP_OAUTH_ENCRYPTION_KEY rotated?); re-enter the secrets and save, or revert to env config")

// httpErrorForNotifySettings maps validation failures (shape checks in
// NotifySettingsInput.normalize, reported before anything persists) to 400,
// stored secrets the current key cannot decrypt to 409 with what to do, and
// everything else to an internal error.
func httpErrorForNotifySettings(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidNotifySettings):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrNotifySecretsUndecryptable):
		logInternalError(r.Context(), r, err, 1)
		http.Error(w, errNotifySecretsUnreadable.Error(), http.StatusConflict)
	default:
		writeInternalErrorFromHelper(w, r, err)
	}
}
