-- 036_notify_settings.sql — admin-managed task notification settings.
--
-- Task-completion notifications (email + outbound webhook, internal/notify)
-- were configurable only via FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_ON env
-- vars + a restart. This SINGLETON row (id = 'default') makes them
-- admin-editable from the web admin page and hot-swapped into the running
-- notifier. Precedence: this row (when present) replaces the env-derived
-- config wholesale; deleting it reverts to the env vars. Timing knobs
-- (timeout/retries) deliberately stay env-only.
--
--   smtp_password_sealed / webhook_secret_sealed: secretbox AES-256-GCM
--   ciphertexts (same cipher as remote-MCP OAuth tokens + LLM provider keys,
--   FLEET_MCP_OAUTH_ENCRYPTION_KEY), AAD-bound to this row's id with
--   channel-distinct purposes. NULL = no secret stored. The PLAINTEXT values
--   are write-only through the API — no read endpoint ever returns one.
CREATE TABLE IF NOT EXISTS notify_settings (
    id                    TEXT PRIMARY KEY,
    notify_on             TEXT NOT NULL DEFAULT '',
    smtp_host             TEXT NOT NULL DEFAULT '',
    smtp_port             TEXT NOT NULL DEFAULT '587',
    smtp_username         TEXT NOT NULL DEFAULT '',
    smtp_password_sealed  BYTEA,
    smtp_from             TEXT NOT NULL DEFAULT '',
    email_to              TEXT NOT NULL DEFAULT '',
    webhook_url           TEXT NOT NULL DEFAULT '',
    webhook_method        TEXT NOT NULL DEFAULT 'POST',
    webhook_body_template TEXT NOT NULL DEFAULT '',
    webhook_secret_sealed BYTEA,
    updated_at            BIGINT NOT NULL,
    updated_by            TEXT NOT NULL DEFAULT ''
);
