package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Admin notification settings (migration 036). Load-bearing assertions:
// secrets seal/decrypt round-trip with channel-distinct AAD, the write-only
// convention (nil keeps, "" clears), the UI read never carries a value,
// validation rejects junk before SQL, and keyed writes fail closed without a
// cipher.
func TestNotifySettingsCRUDAndSecretSealing(t *testing.T) {
	s := newTestStoreWithCipher(t)
	ctx := context.Background()

	if _, err := s.GetNotifySettings(ctx); !errors.Is(err, ErrNotifySettingsNotFound) {
		t.Fatalf("fresh table: want ErrNotifySettingsNotFound, got %v", err)
	}

	pass, secret := "smtp-p@ss", "hook-secret"
	saved, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{
		NotifyOn: "Failure, success", SMTPHost: "smtp.example.com", SMTPPort: "465",
		SMTPUsername: "fleet", SMTPPassword: &pass, SMTPFrom: "fleet@example.com",
		EmailTo: "ops@example.com", WebhookURL: "https://hooks.example.com/x",
		WebhookMethod: "post", WebhookSecret: &secret,
	}, "admin@x.com")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !saved.HasSMTPPassword || !saved.HasWebhookSecret || saved.UpdatedBy != "admin@x.com" {
		t.Errorf("saved = %+v", saved)
	}
	if saved.NotifyOn != "failure, success" || saved.WebhookMethod != "POST" {
		t.Errorf("normalization: %+v", saved)
	}

	// Decrypting read (host-side only) round-trips both secrets.
	cfg, err := s.GetNotifySettingsConfig(ctx)
	if err != nil {
		t.Fatalf("config read: %v", err)
	}
	if cfg.SMTPPassword != pass || cfg.WebhookSecret != secret {
		t.Fatalf("decrypted secrets do not round-trip")
	}

	// nil secrets on update keep the stored ciphertexts; "" clears one.
	empty := ""
	saved, err = s.UpsertNotifySettings(ctx, NotifySettingsInput{
		SMTPHost: "smtp.example.com", EmailTo: "ops@example.com",
		WebhookURL: "https://hooks.example.com/x", WebhookSecret: &empty,
	}, "other@x.com")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !saved.HasSMTPPassword {
		t.Error("nil password must keep the stored value")
	}
	if saved.HasWebhookSecret {
		t.Error("empty webhook secret must clear the stored value")
	}
	cfg, err = s.GetNotifySettingsConfig(ctx)
	if err != nil {
		t.Fatalf("config re-read: %v", err)
	}
	if cfg.SMTPPassword != pass || cfg.WebhookSecret != "" {
		t.Errorf("after keep/clear: pass kept=%v secret cleared=%v", cfg.SMTPPassword == pass, cfg.WebhookSecret == "")
	}

	// Validation rejects junk before SQL.
	if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{NotifyOn: "sometimes"}, "a@x"); !errors.Is(err, ErrInvalidNotifySettings) {
		t.Errorf("bad notify_on: %v", err)
	}
	if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{WebhookURL: "ftp://x"}, "a@x"); !errors.Is(err, ErrInvalidNotifySettings) {
		t.Errorf("bad webhook_url: %v", err)
	}
	if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{WebhookMethod: "DELETE"}, "a@x"); !errors.Is(err, ErrInvalidNotifySettings) {
		t.Errorf("bad method: %v", err)
	}
	if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{SMTPPort: "abc"}, "a@x"); !errors.Is(err, ErrInvalidNotifySettings) {
		t.Errorf("bad port: %v", err)
	}

	// Delete reverts to env (not-found again); idempotent.
	if err := s.DeleteNotifySettings(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteNotifySettings(ctx); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := s.GetNotifySettings(ctx); !errors.Is(err, ErrNotifySettingsNotFound) {
		t.Fatalf("after delete: want not found, got %v", err)
	}
}

// TestNotifySettingsSecretsFailClosedWithoutCipher: a keyed write without the
// store cipher is rejected with an actionable error; secret-free writes work.
func TestNotifySettingsSecretsFailClosedWithoutCipher(t *testing.T) {
	s := newTestStore(t) // no cipher installed
	ctx := context.Background()

	pass := "p"
	_, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{SMTPHost: "h", SMTPPassword: &pass}, "a@x")
	if err == nil || !strings.Contains(err.Error(), "FLEET_MCP_OAUTH_ENCRYPTION_KEY") {
		t.Fatalf("keyed write without cipher should fail closed with guidance, got %v", err)
	}
	if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{
		WebhookURL: "https://hooks.example.com/x",
	}, "a@x"); err != nil {
		t.Fatalf("secret-free write should work without a cipher: %v", err)
	}
}
