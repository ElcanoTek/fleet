package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestNotifySettingsNormalizeAcceptsBlocked: "blocked" is a notify_on value
// the panel may save (ADR-0092); unknown values are still refused.
func TestNotifySettingsNormalizeAcceptsBlocked(t *testing.T) {
	in := NotifySettingsInput{NotifyOn: " Blocked,failure "}
	if err := in.Normalize(); err != nil {
		t.Fatalf("Normalize(blocked,failure): %v", err)
	}
	if in.NotifyOn != "blocked,failure" {
		t.Errorf("notify_on = %q", in.NotifyOn)
	}
	bad := NotifySettingsInput{NotifyOn: "blocked,stalled"}
	if err := bad.Normalize(); !errors.Is(err, ErrInvalidNotifySettings) || !strings.Contains(err.Error(), "success|failure|blocked|progress|always") {
		t.Errorf("Normalize(stalled) = %v", err)
	}
}

// TestNotifySettingsOwnerDigestFields: the digest settings (migration 083)
// default to off at 13:00 UTC, round-trip through the row, and an hour
// outside 0-23 is refused before anything persists.
func TestNotifySettingsOwnerDigestFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	saved, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{SMTPHost: "smtp.example.com"}, "a@x")
	if err != nil {
		t.Fatalf("upsert defaults: %v", err)
	}
	if saved.OwnerDigestEnabled || saved.OwnerDigestHourUTC != DefaultOwnerDigestHourUTC {
		t.Fatalf("defaults = enabled %v hour %d, want off at %d", saved.OwnerDigestEnabled, saved.OwnerDigestHourUTC, DefaultOwnerDigestHourUTC)
	}

	zero := 0
	saved, err = s.UpsertNotifySettings(ctx, NotifySettingsInput{
		SMTPHost: "smtp.example.com", OwnerDigestEnabled: true, OwnerDigestHourUTC: &zero,
	}, "a@x")
	if err != nil {
		t.Fatalf("upsert digest: %v", err)
	}
	if !saved.OwnerDigestEnabled || saved.OwnerDigestHourUTC != 0 {
		t.Fatalf("saved = enabled %v hour %d, want on at 0 (midnight is a real hour, not unset)", saved.OwnerDigestEnabled, saved.OwnerDigestHourUTC)
	}
	cfg, err := s.GetNotifySettingsConfig(ctx)
	if err != nil {
		t.Fatalf("config read: %v", err)
	}
	if !cfg.OwnerDigestEnabled || cfg.OwnerDigestHourUTC != 0 {
		t.Fatalf("config read = %+v", cfg.NotifySettings)
	}

	for _, bad := range []int{-1, 24, 99} {
		h := bad
		if _, err := s.UpsertNotifySettings(ctx, NotifySettingsInput{OwnerDigestEnabled: true, OwnerDigestHourUTC: &h}, "a@x"); !errors.Is(err, ErrInvalidNotifySettings) {
			t.Errorf("hour %d: want ErrInvalidNotifySettings, got %v", bad, err)
		}
	}
	if got, _ := s.GetNotifySettings(ctx); !got.OwnerDigestEnabled || got.OwnerDigestHourUTC != 0 {
		t.Errorf("a refused write changed the row: %+v", got)
	}
}

// TestClaimOwnerDigestDayOncePerDay: the first claim of a UTC day wins and
// every later one — another replica, or this process after a restart — loses;
// a new day can be claimed again. Concurrent claimers race to one winner.
func TestClaimOwnerDigestDayOncePerDay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	day := time.Date(2026, 10, 23, 13, 5, 0, 0, time.UTC)

	won, err := s.ClaimOwnerDigestDay(ctx, day)
	if err != nil || !won {
		t.Fatalf("first claim = %v, %v; want won", won, err)
	}
	// Same UTC day, later hour, and the same instant seen from another zone.
	for _, again := range []time.Time{day.Add(5 * time.Hour), day.In(time.FixedZone("UTC-11", -11*3600))} {
		if won, err := s.ClaimOwnerDigestDay(ctx, again); err != nil || won {
			t.Fatalf("second claim for %v = %v, %v; want lost", again, won, err)
		}
	}
	// The next UTC day is a fresh claim.
	if won, err := s.ClaimOwnerDigestDay(ctx, day.Add(24*time.Hour)); err != nil || !won {
		t.Fatalf("next day claim = %v, %v; want won", won, err)
	}

	// Concurrent claimers for a new day: exactly one wins.
	race := day.Add(48 * time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := s.ClaimOwnerDigestDay(ctx, race)
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent claims won %d times, want exactly 1", wins)
	}

	// Old days are pruned by the claim that inserts a new one.
	if won, err := s.ClaimOwnerDigestDay(ctx, day.AddDate(0, 0, ownerDigestRetainDays+10)); err != nil || !won {
		t.Fatalf("far-future claim = %v, %v", won, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM owner_digest_days WHERE day = $1::date`, day.Format(time.DateOnly)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a claim %d days old was not pruned", ownerDigestRetainDays+10)
	}
}
