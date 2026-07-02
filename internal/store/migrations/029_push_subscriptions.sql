-- 029_push_subscriptions.sql — browser Web Push subscriptions (#292).
--
-- One row per (user, browser) PushSubscription. The endpoint is the push
-- relay's unique per-browser URL, so it is the natural upsert key; keys_auth /
-- keys_p256dh are the base64url client keys the Web Push protocol (RFC 8291)
-- encrypts payloads against — the relay never sees plaintext. These are NOT
-- fleet credentials: they only let this server send to that one browser, and a
-- 404/410 from the relay retires the row (see internal/webpush).
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id             TEXT PRIMARY KEY,
    user_email     TEXT NOT NULL,
    endpoint       TEXT NOT NULL UNIQUE,
    keys_auth      TEXT NOT NULL,
    keys_p256dh    TEXT NOT NULL,
    created_at     BIGINT NOT NULL,
    last_active_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user_email ON push_subscriptions (user_email);
