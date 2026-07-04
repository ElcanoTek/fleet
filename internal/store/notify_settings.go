package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// Admin-managed task notification settings (migration 036): one singleton row
// that, when present, replaces the FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_ON
// env config wholesale. Secrets (the SMTP password and the outbound webhook
// signing secret) are sealed under the store cipher and are WRITE-ONLY through
// the API — the UI-facing read carries has_* booleans only, and the one
// decrypting read exists solely to build the live notifier / run the admin
// test host-side. Mirrors internal/store/llm_providers.go.

const (
	// notifySettingsID is the singleton row id — notification config is
	// deployment-wide, one row, ever.
	notifySettingsID = "default"

	//nolint:gosec // G101: AAD domain-separation labels, not credentials.
	aadPurposeNotifySMTPPassword  = "fleet:notify-smtp-password:v1"
	aadPurposeNotifyWebhookSecret = "fleet:notify-webhook-secret:v1"
)

// ErrNotifySettingsNotFound is returned when no admin row exists (env config
// is in effect). ErrInvalidNotifySettings wraps every shape-validation failure
// in NotifySettingsInput.normalize — reported BEFORE anything persists, so the
// API layer can map it to 400.
var (
	ErrNotifySettingsNotFound = errors.New("notify settings not configured")
	ErrInvalidNotifySettings  = errors.New("invalid notify settings")
	// ErrNotifySecretsUndecryptable marks a row whose sealed secrets cannot be
	// opened (the encryption key was rotated or lost). The row still reads
	// fine without decryption, so the admin panel can stay up and offer the
	// two recovery paths: re-enter the secrets, or revert to env config.
	ErrNotifySecretsUndecryptable = errors.New("stored notification secrets cannot be decrypted (was FLEET_MCP_OAUTH_ENCRYPTION_KEY rotated?)")
)

// NotifySettings is the UI-facing shape: no secret values, ever.
type NotifySettings struct {
	NotifyOn            string `json:"notify_on"`
	SMTPHost            string `json:"smtp_host"`
	SMTPPort            string `json:"smtp_port"`
	SMTPUsername        string `json:"smtp_username"`
	HasSMTPPassword     bool   `json:"has_smtp_password"`
	SMTPFrom            string `json:"smtp_from"`
	EmailTo             string `json:"email_to"`
	WebhookURL          string `json:"webhook_url"`
	WebhookMethod       string `json:"webhook_method"`
	WebhookBodyTemplate string `json:"webhook_body_template"`
	HasWebhookSecret    bool   `json:"has_webhook_secret"`
	UpdatedAt           int64  `json:"updated_at"`
	UpdatedBy           string `json:"updated_by"`
}

// NotifySettingsConfig is NotifySettings plus the DECRYPTED secrets — host-side
// use only (building the live notifier, running the admin test). Never
// serialize it to HTTP.
type NotifySettingsConfig struct {
	NotifySettings
	SMTPPassword  string `json:"-"`
	WebhookSecret string `json:"-"`
}

// NotifySettingsInput is the write payload. The secret fields follow the
// write-only convention: nil = leave the stored value unchanged, "" = clear.
type NotifySettingsInput struct {
	NotifyOn            string
	SMTPHost            string
	SMTPPort            string
	SMTPUsername        string
	SMTPPassword        *string
	SMTPFrom            string
	EmailTo             string
	WebhookURL          string
	WebhookMethod       string
	WebhookBodyTemplate string
	WebhookSecret       *string
}

// Normalize trims and shape-checks the non-secret fields. Deep validation
// (does the SMTP host answer, is the webhook reachable) belongs to the Test
// button, not the write path.
func (in *NotifySettingsInput) Normalize() error {
	in.NotifyOn = strings.TrimSpace(strings.ToLower(in.NotifyOn))
	for _, part := range strings.Split(in.NotifyOn, ",") {
		switch strings.TrimSpace(part) {
		case "", "success", "failure", "progress", "always":
		default:
			return fmt.Errorf("%w: notify_on value %q (want a CSV of success|failure|progress|always, or empty for all terminal statuses)", ErrInvalidNotifySettings, strings.TrimSpace(part))
		}
	}
	in.SMTPHost = strings.TrimSpace(in.SMTPHost)
	in.SMTPPort = strings.TrimSpace(in.SMTPPort)
	if in.SMTPPort == "" {
		in.SMTPPort = "587"
	}
	for _, c := range in.SMTPPort {
		if c < '0' || c > '9' {
			return fmt.Errorf("%w: smtp_port %q (want a number)", ErrInvalidNotifySettings, in.SMTPPort)
		}
	}
	in.SMTPUsername = strings.TrimSpace(in.SMTPUsername)
	in.SMTPFrom = strings.TrimSpace(in.SMTPFrom)
	in.EmailTo = strings.TrimSpace(in.EmailTo)
	in.WebhookURL = strings.TrimSpace(in.WebhookURL)
	if in.WebhookURL != "" && !strings.HasPrefix(in.WebhookURL, "http://") && !strings.HasPrefix(in.WebhookURL, "https://") {
		return fmt.Errorf("%w: webhook_url must be http:// or https://", ErrInvalidNotifySettings)
	}
	in.WebhookMethod = strings.ToUpper(strings.TrimSpace(in.WebhookMethod))
	if in.WebhookMethod == "" {
		in.WebhookMethod = "POST"
	}
	switch in.WebhookMethod {
	case "POST", "PUT", "PATCH":
	default:
		return fmt.Errorf("%w: webhook_method %q (want POST, PUT, or PATCH)", ErrInvalidNotifySettings, in.WebhookMethod)
	}
	return nil
}

// GetNotifySettings returns the UI-facing singleton row.
// ErrNotifySettingsNotFound when no admin row exists.
func (s *Store) GetNotifySettings(ctx context.Context) (*NotifySettings, error) {
	c, err := s.getNotifySettings(ctx, false)
	if err != nil {
		return nil, err
	}
	return &c.NotifySettings, nil
}

// GetNotifySettingsConfig returns the singleton row WITH decrypted secrets —
// host-side use only (live notifier build + admin test). Never expose over HTTP.
func (s *Store) GetNotifySettingsConfig(ctx context.Context) (*NotifySettingsConfig, error) {
	return s.getNotifySettings(ctx, true)
}

func (s *Store) getNotifySettings(ctx context.Context, decrypt bool) (*NotifySettingsConfig, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT notify_on, smtp_host, smtp_port, smtp_username, smtp_password_sealed,
		       smtp_from, email_to, webhook_url, webhook_method, webhook_body_template,
		       webhook_secret_sealed, updated_at, updated_by
		FROM notify_settings WHERE id = $1`, notifySettingsID)
	var c NotifySettingsConfig
	var sealedPass, sealedSecret []byte
	err := row.Scan(&c.NotifyOn, &c.SMTPHost, &c.SMTPPort, &c.SMTPUsername, &sealedPass,
		&c.SMTPFrom, &c.EmailTo, &c.WebhookURL, &c.WebhookMethod, &c.WebhookBodyTemplate,
		&sealedSecret, &c.UpdatedAt, &c.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotifySettingsNotFound
	}
	if err != nil {
		return nil, err
	}
	c.HasSMTPPassword = len(sealedPass) > 0
	c.HasWebhookSecret = len(sealedSecret) > 0
	if decrypt {
		if c.SMTPPassword, err = s.openNotifySecret(sealedPass, aadPurposeNotifySMTPPassword); err != nil {
			return nil, fmt.Errorf("%w: smtp password: %w", ErrNotifySecretsUndecryptable, err)
		}
		if c.WebhookSecret, err = s.openNotifySecret(sealedSecret, aadPurposeNotifyWebhookSecret); err != nil {
			return nil, fmt.Errorf("%w: webhook secret: %w", ErrNotifySecretsUndecryptable, err)
		}
	}
	return &c, nil
}

// UpsertNotifySettings writes the singleton row. Secret semantics: nil = keep
// the stored ciphertext, "" = clear, non-empty = seal and replace. Storing a
// secret without a cipher configured fails closed.
func (s *Store) UpsertNotifySettings(ctx context.Context, in NotifySettingsInput, updatedBy string) (*NotifySettings, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	// Resolve the secret columns: existing ciphertext (keep), NULL (clear), or
	// a fresh seal.
	var curPass, curSecret []byte
	row := s.db.QueryRowContext(ctx,
		`SELECT smtp_password_sealed, webhook_secret_sealed FROM notify_settings WHERE id = $1`,
		notifySettingsID)
	if err := row.Scan(&curPass, &curSecret); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	sealedPass, err := s.resolveNotifySecret(in.SMTPPassword, curPass, aadPurposeNotifySMTPPassword)
	if err != nil {
		return nil, err
	}
	sealedSecret, err := s.resolveNotifySecret(in.WebhookSecret, curSecret, aadPurposeNotifyWebhookSecret)
	if err != nil {
		return nil, err
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO notify_settings (id, notify_on, smtp_host, smtp_port, smtp_username,
			smtp_password_sealed, smtp_from, email_to, webhook_url, webhook_method,
			webhook_body_template, webhook_secret_sealed, updated_at, updated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE SET
			notify_on = EXCLUDED.notify_on, smtp_host = EXCLUDED.smtp_host,
			smtp_port = EXCLUDED.smtp_port, smtp_username = EXCLUDED.smtp_username,
			smtp_password_sealed = EXCLUDED.smtp_password_sealed,
			smtp_from = EXCLUDED.smtp_from, email_to = EXCLUDED.email_to,
			webhook_url = EXCLUDED.webhook_url, webhook_method = EXCLUDED.webhook_method,
			webhook_body_template = EXCLUDED.webhook_body_template,
			webhook_secret_sealed = EXCLUDED.webhook_secret_sealed,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		notifySettingsID, in.NotifyOn, in.SMTPHost, in.SMTPPort, in.SMTPUsername,
		sealedPass, in.SMTPFrom, in.EmailTo, in.WebhookURL, in.WebhookMethod,
		in.WebhookBodyTemplate, sealedSecret, time.Now().Unix(), updatedBy)
	if err != nil {
		return nil, err
	}
	return s.GetNotifySettings(ctx)
}

// DeleteNotifySettings removes the admin row, reverting notifications to the
// env-derived config. Idempotent.
func (s *Store) DeleteNotifySettings(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_settings WHERE id = $1`, notifySettingsID)
	return err
}

// resolveNotifySecret maps the write-only input convention onto the stored
// ciphertext: nil input keeps cur, empty clears, non-empty seals fresh.
func (s *Store) resolveNotifySecret(in *string, cur []byte, purpose string) ([]byte, error) {
	if in == nil {
		return cur, nil
	}
	v := strings.TrimSpace(*in)
	if v == "" {
		return nil, nil
	}
	if s.tokenCipher == nil {
		return nil, fmt.Errorf("storing a notification secret requires FLEET_MCP_OAUTH_ENCRYPTION_KEY: %w", secretbox.ErrNoCipher)
	}
	return s.tokenCipher.Seal([]byte(v), secretbox.AAD(purpose, notifySettingsID))
}

// openNotifySecret decrypts one sealed secret (nil/empty → "").
func (s *Store) openNotifySecret(sealed []byte, purpose string) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if s.tokenCipher == nil {
		return "", secretbox.ErrNoCipher
	}
	pt, err := s.tokenCipher.Open(sealed, secretbox.AAD(purpose, notifySettingsID))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
