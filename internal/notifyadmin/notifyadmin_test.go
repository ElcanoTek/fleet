package notifyadmin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// fakeStore is an in-memory Store seam; the real sealed-secret store methods
// have their own DB-gated tests in internal/store.
type fakeStore struct {
	row *store.NotifySettingsConfig
	// undecryptable simulates a row sealed under a rotated/lost key: the plain
	// read works, the decrypting read fails.
	undecryptable bool
}

func (f *fakeStore) GetNotifySettings(context.Context) (*store.NotifySettings, error) {
	if f.row == nil {
		return nil, store.ErrNotifySettingsNotFound
	}
	ns := f.row.NotifySettings
	return &ns, nil
}

func (f *fakeStore) GetNotifySettingsConfig(context.Context) (*store.NotifySettingsConfig, error) {
	if f.row == nil {
		return nil, store.ErrNotifySettingsNotFound
	}
	if f.undecryptable {
		return nil, fmt.Errorf("%w: smtp password: cipher: message authentication failed", store.ErrNotifySecretsUndecryptable)
	}
	c := *f.row
	return &c, nil
}

func (f *fakeStore) UpsertNotifySettings(_ context.Context, in store.NotifySettingsInput, updatedBy string) (*store.NotifySettings, error) {
	cur := &store.NotifySettingsConfig{}
	if f.row != nil {
		cur = f.row
	}
	next := &store.NotifySettingsConfig{
		NotifySettings: store.NotifySettings{
			NotifyOn: in.NotifyOn, SMTPHost: in.SMTPHost, SMTPPort: in.SMTPPort,
			SMTPUsername: in.SMTPUsername, SMTPFrom: in.SMTPFrom, EmailTo: in.EmailTo,
			WebhookURL: in.WebhookURL, WebhookMethod: in.WebhookMethod,
			WebhookBodyTemplate: in.WebhookBodyTemplate,
			UpdatedAt:           42, UpdatedBy: updatedBy,
		},
		SMTPPassword:  cur.SMTPPassword,
		WebhookSecret: cur.WebhookSecret,
	}
	// Write-only secret convention: nil = keep, "" = clear, value = replace.
	if in.SMTPPassword != nil {
		next.SMTPPassword = *in.SMTPPassword
	}
	if in.WebhookSecret != nil {
		next.WebhookSecret = *in.WebhookSecret
	}
	next.HasSMTPPassword = next.SMTPPassword != ""
	next.HasWebhookSecret = next.WebhookSecret != ""
	f.row = next
	ns := next.NotifySettings
	return &ns, nil
}

func (f *fakeStore) DeleteNotifySettings(context.Context) error {
	f.row = nil
	return nil
}

// fakeSwapper records every applied config.
type fakeSwapper struct {
	applied []notify.Config
}

func (f *fakeSwapper) SetConfig(cfg notify.Config) { f.applied = append(f.applied, cfg) }

func envConfig() notify.Config {
	return notify.Config{
		SMTPHost: "smtp.env.example", SMTPPort: "587", SMTPFrom: "fleet@env.example",
		SMTPPassword: "env-smtp-secret", EmailTo: []string{"ops@env.example"},
		PublicURLBase: "https://fleet.example",
	}
}

// TestViewEnvAndSecretsNeverLeak: with no admin row the env config renders as
// the view — secrets as booleans only, never values.
func TestViewEnvAndSecretsNeverLeak(t *testing.T) {
	svc := NewService(&fakeStore{}, envConfig(), &fakeSwapper{})
	v, err := svc.View(context.Background())
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if v.Source != SourceEnv || v.Settings.SMTPHost != "smtp.env.example" || !v.Settings.HasSMTPPassword {
		t.Errorf("env view = %+v", v)
	}
	if !v.EmailEnabled || v.WebhookEnabled {
		t.Errorf("env config has email only: %+v", v)
	}
}

// TestSaveAppliesAndRevertRestoresEnv: a save swaps the row-derived config into
// the live notifier (inheriting env timing + URL base); revert swaps env back.
func TestSaveAppliesAndRevertRestoresEnv(t *testing.T) {
	st := &fakeStore{}
	sw := &fakeSwapper{}
	svc := NewService(st, envConfig(), sw)
	ctx := context.Background()

	secret := "hook-secret"
	v, err := svc.Save(ctx, store.NotifySettingsInput{
		NotifyOn:      "failure",
		WebhookURL:    "https://hooks.example/fleet",
		WebhookMethod: "POST",
		WebhookSecret: &secret,
	}, "admin@x.com")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if v.Source != SourceAdmin || !v.WebhookEnabled || v.EmailEnabled {
		t.Errorf("saved view = %+v, want admin webhook-only", v)
	}
	if !v.Settings.HasWebhookSecret {
		t.Error("saved secret should surface as has_webhook_secret")
	}
	if len(sw.applied) != 1 {
		t.Fatalf("save should apply once, got %d", len(sw.applied))
	}
	applied := sw.applied[0]
	if applied.WebhookURL != "https://hooks.example/fleet" || applied.WebhookSecret != "hook-secret" {
		t.Errorf("applied config = %+v", applied)
	}
	// The admin row replaces channel config WHOLESALE (env SMTP is gone) but
	// inherits the env public URL base.
	if applied.EmailConfigured() {
		t.Error("admin row without SMTP must not inherit env email config")
	}
	if applied.PublicURLBase != "https://fleet.example" {
		t.Errorf("public URL base should inherit from env, got %q", applied.PublicURLBase)
	}
	if got := applied.On; len(got) != 1 || got[0] != "failure" {
		t.Errorf("On = %v", got)
	}

	rv, err := svc.Revert(ctx, "admin@x.com")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if rv.Source != SourceEnv || !rv.EmailEnabled {
		t.Errorf("revert view = %+v, want env email config back", rv)
	}
	if len(sw.applied) != 2 || sw.applied[1].SMTPHost != "smtp.env.example" {
		t.Fatalf("revert should re-apply the env config, applied=%d", len(sw.applied))
	}
}

// TestApplyBoot: a persisted row applies at boot; no row is a no-op.
func TestApplyBoot(t *testing.T) {
	sw := &fakeSwapper{}
	svc := NewService(&fakeStore{}, envConfig(), sw)
	if err := svc.ApplyBoot(context.Background()); err != nil {
		t.Fatalf("ApplyBoot no row: %v", err)
	}
	if len(sw.applied) != 0 {
		t.Fatal("no row must not swap (env config already serves)")
	}

	st := &fakeStore{row: &store.NotifySettingsConfig{
		NotifySettings: store.NotifySettings{WebhookURL: "https://hooks.example/x", WebhookMethod: "POST", UpdatedBy: "admin@x.com"},
		WebhookSecret:  "s3",
	}}
	svc = NewService(st, envConfig(), sw)
	if err := svc.ApplyBoot(context.Background()); err != nil {
		t.Fatalf("ApplyBoot: %v", err)
	}
	if len(sw.applied) != 1 || sw.applied[0].WebhookSecret != "s3" {
		t.Fatalf("boot apply should swap the decrypted row config")
	}
}

// TestSendTestUsesEffectiveConfig: the Test button drives one REAL webhook
// delivery (against httptest) using the admin row, and reports honest failures
// for an unconfigured channel.
func TestSendTestUsesEffectiveConfig(t *testing.T) {
	var got struct {
		signature string
		body      string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		got.body = string(b)
		got.signature = r.Header.Get("X-Fleet-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st := &fakeStore{row: &store.NotifySettingsConfig{
		NotifySettings: store.NotifySettings{WebhookURL: srv.URL, WebhookMethod: "POST"},
		WebhookSecret:  "test-signing-secret",
	}}
	svc := NewService(st, notify.Config{}, &fakeSwapper{})

	res, err := svc.Test(context.Background(), "webhook")
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !res.OK {
		t.Fatalf("webhook test should succeed: %+v", res)
	}
	if !strings.HasPrefix(got.signature, "v1=") {
		t.Errorf("test webhook should be signed with the stored secret, got %q", got.signature)
	}
	if !strings.Contains(got.body, "Test notification") {
		t.Errorf("test body = %q", got.body)
	}

	// Unconfigured channel: honest failure, no panic, key-free detail.
	res, err = svc.Test(context.Background(), "email")
	if err != nil {
		t.Fatalf("Test email: %v", err)
	}
	if res.OK || !strings.Contains(res.Detail, "not configured") {
		t.Errorf("unconfigured email test = %+v", res)
	}
	if strings.Contains(res.Detail, "test-signing-secret") {
		t.Error("detail must never carry a secret")
	}
}

// TestUndecryptableRowDegradesButStaysRecoverable: a row sealed under a
// rotated key must NOT take the panel down — ApplyBoot leaves env serving,
// View reports Degraded (with env-derived channel status), and both recovery
// paths (Save with fresh secrets, Revert) still work.
func TestUndecryptableRowDegradesButStaysRecoverable(t *testing.T) {
	st := &fakeStore{
		row: &store.NotifySettingsConfig{
			NotifySettings: store.NotifySettings{WebhookURL: "https://hooks.example/x", HasWebhookSecret: true},
		},
		undecryptable: true,
	}
	sw := &fakeSwapper{}
	svc := NewService(st, envConfig(), sw)
	ctx := context.Background()

	if err := svc.ApplyBoot(ctx); err != nil {
		t.Fatalf("ApplyBoot must not fail on an undecryptable row: %v", err)
	}
	if len(sw.applied) != 0 {
		t.Fatal("an undecryptable row must not be applied")
	}

	v, err := svc.View(ctx)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if v.Degraded == "" || v.Source != SourceAdmin {
		t.Errorf("view should surface the degraded admin row: %+v", v)
	}
	// Channel status reflects what actually serves (the env config).
	if !v.EmailEnabled || v.WebhookEnabled {
		t.Errorf("degraded view must report env enablement: %+v", v)
	}

	// Recovery path 1: revert to env config.
	rv, err := svc.Revert(ctx, "admin@x.com")
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if rv.Source != SourceEnv || rv.Degraded != "" {
		t.Errorf("revert should clear the degraded state: %+v", rv)
	}
}
