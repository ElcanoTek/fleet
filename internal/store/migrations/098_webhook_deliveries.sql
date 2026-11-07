-- 098_webhook_deliveries.sql — one row per accepted webhook delivery
-- (docs/WEBHOOKS.md, "Delivery idempotency").
--
-- Senders deliver at least once: Slack retries an event it did not see answered
-- in time, GitHub's "Redeliver" re-sends a delivery with the same id. POST
-- /webhooks/{slug} claims (slug, delivery_key) before it starts a conversation,
-- so a repeat of an accepted delivery answers with the original
-- conversation_id instead of starting a second turn.
--
-- delivery_key is "<source>:<sha256 hex>" (source = github | slack | body).
-- conversation_id is '' while the claiming request is still creating the
-- conversation. No FK to conversations: deleting the conversation must not make
-- a later repeat of the same delivery start a new one. Rows are kept for 24 h,
-- then removed by the retention sweep (Store.SweepWebhookDeliveries).
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    slug            TEXT   NOT NULL,
    delivery_key    TEXT   NOT NULL,
    conversation_id TEXT   NOT NULL DEFAULT '',
    created_at      BIGINT NOT NULL,
    PRIMARY KEY (slug, delivery_key)
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_created
    ON webhook_deliveries (created_at);
