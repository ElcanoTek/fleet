package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/notifyadmin"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Admin notification-settings endpoints. Load-bearing assertions: admin-gated,
// 501 when unwired, secrets never appear in any response, the write-only
// convention reaches the service intact, validation maps to 400, and the test
// endpoint returns a key-free result.

type fakeNotifyService struct {
	view     notifyadmin.View
	lastIn   *store.NotifySettingsInput
	reverted bool
}

func (f *fakeNotifyService) View(context.Context) (notifyadmin.View, error) { return f.view, nil }

func (f *fakeNotifyService) Save(_ context.Context, in store.NotifySettingsInput, updatedBy string) (notifyadmin.View, error) {
	if err := (&in).Normalize(); err != nil {
		return notifyadmin.View{}, err
	}
	f.lastIn = &in
	f.view = notifyadmin.View{Source: notifyadmin.SourceAdmin, Settings: store.NotifySettings{
		SMTPHost: in.SMTPHost, HasSMTPPassword: in.SMTPPassword != nil && *in.SMTPPassword != "",
		WebhookURL: in.WebhookURL, UpdatedBy: updatedBy,
	}}
	return f.view, nil
}

func (f *fakeNotifyService) Revert(context.Context, string) (notifyadmin.View, error) {
	f.reverted = true
	f.view = notifyadmin.View{Source: notifyadmin.SourceEnv}
	return f.view, nil
}

func (f *fakeNotifyService) Test(_ context.Context, channel string) (notify.TestResult, error) {
	return notify.TestResult{OK: channel == "webhook", Detail: "test " + channel, LatencyMS: 3}, nil
}

func notifyFixture(t *testing.T) (http.Handler, *fakeNotifyService) {
	t.Helper()
	s := memberFixture(t, "boss@x.com", "user@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	svc := &fakeNotifyService{view: notifyadmin.View{Source: notifyadmin.SourceEnv}}
	s.notifySettings = svc
	return s.Routes(), svc
}

func TestAdminNotifySettingsGateAnd501(t *testing.T) {
	h, _ := notifyFixture(t)
	if w := do(t, h, http.MethodGet, "/admin/notify-settings", nil, "user@x.com"); w.Code != http.StatusForbidden {
		t.Fatalf("member GET: %d want 403", w.Code)
	}

	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	unwired := s.Routes()
	if w := do(t, unwired, http.MethodGet, "/admin/notify-settings", nil, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired GET: %d want 501", w.Code)
	}
	if w := do(t, unwired, http.MethodPost, "/admin/notify-settings/test",
		map[string]any{"channel": "email"}, "boss@x.com"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired test: %d want 501", w.Code)
	}
}

func TestAdminNotifySettingsRoundTripWriteOnlySecrets(t *testing.T) {
	h, svc := notifyFixture(t)

	// Save with a secret: the value reaches the service, never the response.
	w := do(t, h, http.MethodPut, "/admin/notify-settings", map[string]any{
		"smtp_host": "smtp.example.com", "email_to": "ops@example.com",
		"smtp_password": "super-secret-smtp",
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "super-secret-smtp") {
		t.Fatal("response echoed the secret")
	}
	if svc.lastIn == nil || svc.lastIn.SMTPPassword == nil || *svc.lastIn.SMTPPassword != "super-secret-smtp" {
		t.Fatal("secret did not reach the service")
	}
	var view notifyadmin.View
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Source != notifyadmin.SourceAdmin || !view.Settings.HasSMTPPassword || view.Settings.UpdatedBy != "boss@x.com" {
		t.Errorf("view = %+v", view)
	}

	// Absent secret field on a later PUT = nil (keep stored).
	w = do(t, h, http.MethodPut, "/admin/notify-settings", map[string]any{
		"smtp_host": "smtp.example.com", "email_to": "ops@example.com",
	}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 2: %d", w.Code)
	}
	if svc.lastIn.SMTPPassword != nil {
		t.Error("absent secret field must arrive as nil (keep)")
	}

	// Validation failure → 400.
	if w := do(t, h, http.MethodPut, "/admin/notify-settings",
		map[string]any{"webhook_url": "ftp://nope"}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid URL: %d want 400", w.Code)
	}

	// Revert → env view.
	w = do(t, h, http.MethodDelete, "/admin/notify-settings", nil, "boss@x.com")
	if w.Code != http.StatusOK || !svc.reverted {
		t.Fatalf("DELETE: %d reverted=%v", w.Code, svc.reverted)
	}

	// Test endpoint: key-free result.
	w = do(t, h, http.MethodPost, "/admin/notify-settings/test",
		map[string]any{"channel": "webhook"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("test: %d", w.Code)
	}
	var res notify.TestResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode test: %v", err)
	}
	if !res.OK || res.Detail != "test webhook" {
		t.Errorf("test result = %+v", res)
	}
}
