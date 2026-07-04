// Package notifyadmin is the admin-managed task-notification settings service
// (Settings → Admin → Notifications): the runtime-editable layer over
// internal/notify's env-derived config, following the same shape as
// internal/settings (workspace feature settings) and the LLM-provider slice.
//
// Precedence: the notify_settings DB row (when present) replaces the
// FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_ON env config wholesale — a
// half-merged config (env SMTP + admin webhook) would be impossible to reason
// about from the panel. Revert deletes the row and the env config serves
// again. Timing knobs (timeout/retries) and the public URL base stay
// env-derived either way. Every change is hot-swapped into the ONE shared
// *notify.Notifier via SetConfig — the runner pool, budget alerts, and email
// reply-back all pick it up on their next send, no restart.
//
// Secrets (SMTP password, webhook signing secret) are sealed at rest and
// WRITE-ONLY: the View never carries a value, and the one decrypting read
// backs the live swap and the admin Test button, host-side only.
package notifyadmin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Store is the persistence seam (satisfied by *store.Store).
type Store interface {
	GetNotifySettings(ctx context.Context) (*store.NotifySettings, error)
	GetNotifySettingsConfig(ctx context.Context) (*store.NotifySettingsConfig, error)
	UpsertNotifySettings(ctx context.Context, in store.NotifySettingsInput, updatedBy string) (*store.NotifySettings, error)
	DeleteNotifySettings(ctx context.Context) error
}

// Swapper is the live notifier seam (satisfied by *notify.Notifier).
type Swapper interface {
	SetConfig(cfg notify.Config)
}

// SourceAdmin / SourceEnv are the View.Source values.
const (
	SourceAdmin = "admin"
	SourceEnv   = "env"
)

// View is what the admin GET returns: the effective settings, their source,
// and per-channel enablement — secret values never, has_* booleans only.
type View struct {
	// Source is "admin" when the DB row is in effect, else "env".
	Source   string               `json:"source"`
	Settings store.NotifySettings `json:"settings"`
	// EmailEnabled/WebhookEnabled report whether each channel would fire under
	// the effective config, so the panel can show honest per-channel status.
	EmailEnabled   bool `json:"email_enabled"`
	WebhookEnabled bool `json:"webhook_enabled"`
	// Degraded, when non-empty, says why the admin row is NOT in effect (the
	// env config serves instead) — today: sealed secrets that no longer
	// decrypt after a key rotation. The panel shows it with the two recovery
	// paths (re-enter secrets and Save, or Revert), which both work because
	// the panel stays registered in this state.
	Degraded string `json:"degraded,omitempty"`
}

// Service resolves, persists, applies, and tests notification settings.
// Save/Revert/ApplyBoot serialize under one mutex (the same discipline as the
// other admin reloaders).
type Service struct {
	st       Store
	env      notify.Config // env-derived boot config (secrets included, host-side only)
	notifier Swapper
	mu       sync.Mutex
}

// NewService builds the service. env is the notify.Load() result captured at
// boot; notifier is the shared live notifier the swaps target.
func NewService(st Store, env notify.Config, notifier Swapper) *Service {
	return &Service{st: st, env: env, notifier: notifier}
}

// View returns the effective settings for the admin GET.
func (s *Service) View(ctx context.Context) (View, error) {
	row, err := s.st.GetNotifySettings(ctx)
	switch {
	case errors.Is(err, store.ErrNotifySettingsNotFound):
		return s.envView(), nil
	case err != nil:
		return View{}, err
	}
	v := View{
		Source:   SourceAdmin,
		Settings: *row,
	}
	// Probe decryptability so the panel never claims a config is in effect
	// that the notifier could not actually load (e.g. after a key rotation).
	if _, err := s.st.GetNotifySettingsConfig(ctx); errors.Is(err, store.ErrNotifySecretsUndecryptable) {
		v.Degraded = "The saved secrets cannot be decrypted (encryption key changed?). The env-derived config is serving. Re-enter the secrets and save, or revert to env config."
		v.EmailEnabled = s.env.EmailConfigured()
		v.WebhookEnabled = s.env.WebhookConfigured()
		return v, nil
	} else if err != nil {
		return View{}, err
	}
	cfg := s.rowConfig(&store.NotifySettingsConfig{NotifySettings: *row})
	v.EmailEnabled = cfg.EmailConfigured()
	v.WebhookEnabled = cfg.WebhookConfigured()
	return v, nil
}

// envView renders the env-derived config as the panel's starting point —
// non-secret fields verbatim, secrets as has_* booleans only.
func (s *Service) envView() View {
	return View{
		Source: SourceEnv,
		Settings: store.NotifySettings{
			NotifyOn:            strings.Join(s.env.On, ","),
			SMTPHost:            s.env.SMTPHost,
			SMTPPort:            s.env.SMTPPort,
			SMTPUsername:        s.env.SMTPUsername,
			HasSMTPPassword:     s.env.SMTPPassword != "",
			SMTPFrom:            s.env.SMTPFrom,
			EmailTo:             strings.Join(s.env.EmailTo, ","),
			WebhookURL:          s.env.WebhookURL,
			WebhookMethod:       s.env.WebhookMethod,
			WebhookBodyTemplate: s.env.WebhookBodyTemplate,
			HasWebhookSecret:    s.env.WebhookSecret != "",
		},
		EmailEnabled:   s.env.EmailConfigured(),
		WebhookEnabled: s.env.WebhookConfigured(),
	}
}

// Save validates + persists the admin row, hot-swaps the live notifier to the
// new effective config, and returns the fresh view. Validation failures never
// persist; a persisted row always matches what was applied.
func (s *Service) Save(ctx context.Context, in store.NotifySettingsInput, updatedBy string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.st.UpsertNotifySettings(ctx, in, updatedBy); err != nil {
		return View{}, err
	}
	if err := s.applyLocked(ctx); err != nil {
		return View{}, fmt.Errorf("saved, but applying the notifier config failed: %w", err)
	}
	log.Printf("notify settings: admin config saved (by %s)", updatedBy)
	return s.View(ctx)
}

// Revert deletes the admin row and swaps the env-derived config back in.
func (s *Service) Revert(ctx context.Context, updatedBy string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.st.DeleteNotifySettings(ctx); err != nil {
		return View{}, err
	}
	s.notifier.SetConfig(s.env)
	log.Printf("notify settings: reverted to env config (by %s)", updatedBy)
	return s.envView(), nil
}

// Test fires one real delivery attempt of a synthetic event over the named
// channel using the EFFECTIVE config (admin row when present, else env),
// decrypting secrets host-side only. The result is key-free by construction.
func (s *Service) Test(ctx context.Context, channel string) (notify.TestResult, error) {
	cfg, err := s.effectiveConfig(ctx)
	if err != nil {
		return notify.TestResult{}, err
	}
	return notify.RunTest(ctx, cfg, channel), nil
}

// ApplyBoot pushes the persisted admin config (if any) into the live notifier
// at boot. No row → no-op (the notifier was already built from env).
func (s *Service) ApplyBoot(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, err := s.st.GetNotifySettingsConfig(ctx)
	if errors.Is(err, store.ErrNotifySettingsNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrNotifySecretsUndecryptable) {
		// A rotated/lost key must not take the panel down — that would strand
		// the admin with no UI path to re-enter the secrets or revert. Env
		// config keeps serving; View reports the degraded state.
		log.Printf("notify settings: admin config NOT applied — %v; env config serving (re-enter secrets or revert from the admin panel)", err)
		return nil
	}
	if err != nil {
		return err
	}
	s.notifier.SetConfig(s.rowConfig(row))
	log.Printf("notify settings: admin config in effect (set by %s)", row.UpdatedBy)
	return nil
}

// applyLocked re-reads the decrypted row and swaps it in. Callers hold s.mu.
func (s *Service) applyLocked(ctx context.Context) error {
	row, err := s.st.GetNotifySettingsConfig(ctx)
	if err != nil {
		return err
	}
	s.notifier.SetConfig(s.rowConfig(row))
	return nil
}

// effectiveConfig resolves the config a send would use right now.
func (s *Service) effectiveConfig(ctx context.Context) (notify.Config, error) {
	row, err := s.st.GetNotifySettingsConfig(ctx)
	if errors.Is(err, store.ErrNotifySettingsNotFound) {
		return s.env, nil
	}
	if err != nil {
		return notify.Config{}, err
	}
	return s.rowConfig(row), nil
}

// rowConfig maps the DB row onto a notify.Config, inheriting the env config's
// non-admin knobs (public URL base, timeout, retries, backoff) so the admin
// surface stays exactly the channel fields.
func (s *Service) rowConfig(row *store.NotifySettingsConfig) notify.Config {
	cfg := s.env // copy: keeps PublicURLBase + timing knobs (and their set-ness)
	cfg.On = splitCSV(row.NotifyOn)
	cfg.SMTPHost = row.SMTPHost
	cfg.SMTPPort = row.SMTPPort
	cfg.SMTPUsername = row.SMTPUsername
	cfg.SMTPPassword = row.SMTPPassword
	cfg.SMTPFrom = row.SMTPFrom
	cfg.EmailTo = splitCSV(row.EmailTo)
	cfg.WebhookURL = row.WebhookURL
	cfg.WebhookMethod = row.WebhookMethod
	cfg.WebhookBodyTemplate = row.WebhookBodyTemplate
	cfg.WebhookSecret = row.WebhookSecret
	return cfg
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
