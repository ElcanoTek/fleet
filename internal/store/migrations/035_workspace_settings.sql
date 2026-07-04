-- 035_workspace_settings.sql — admin-managed workspace feature settings.
--
-- A small key/value table holding ADMIN OVERRIDES for the curated set of
-- runtime feature settings (internal/settings.Registry): PII redaction mode,
-- tool disclosure threshold, phone-a-friend, memory auto-index, and so on.
-- Precedence is admin row > env var > built-in default: a row here overrides
-- the env-derived boot default; deleting the row reverts to it. Only
-- registered keys are ever written (the API validates against the registry),
-- and every value is a validated plain string ("true", "redact", "65536") —
-- no secrets are stored here (secret-bearing config such as SMTP/webhook
-- credentials deliberately stays out of this table; see docs/ADMIN-SETTINGS.md).
CREATE TABLE IF NOT EXISTS workspace_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at BIGINT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''
);
