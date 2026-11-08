-- Durable outbox for the signed account-events feed (docs/ACCOUNT-EVENTS.md).
-- One row per membership/role change of a Chat account, delivered in id order
-- per email by the serve process. The payload is rebuilt from these columns
-- on every attempt, so a retry sends the same body with a fresh signature.
CREATE TABLE account_events (
    id BIGSERIAL PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL CHECK (type IN ('user.access_changed', 'user.deleted')),
    email TEXT NOT NULL,
    enabled BOOLEAN NOT NULL,
    chat_role TEXT NOT NULL CHECK (chat_role IN ('', 'member', 'viewer', 'admin')),
    ops_role TEXT NOT NULL CHECK (ops_role IN ('', 'none', 'readonly', 'client', 'admin')),
    source TEXT NOT NULL CHECK (source IN ('admin_ui', 'cli', 'system', 'resync', 'identity_provider')),
    actor TEXT NOT NULL DEFAULT '',
    occurred_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at BIGINT NOT NULL,
    lease_until BIGINT,
    delivered_at BIGINT,
    failed_at BIGINT,
    -- When last_error was written: `fleet account-events status` reports the
    -- most recent failure by this, not by event id.
    last_failed_at BIGINT,
    last_error TEXT
);

-- The worker's scan: the oldest undelivered row per email.
CREATE INDEX account_events_pending_idx ON account_events (email, id)
    WHERE delivered_at IS NULL AND failed_at IS NULL;
