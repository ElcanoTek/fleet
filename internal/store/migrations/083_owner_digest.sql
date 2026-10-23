-- 083_owner_digest.sql — the daily owner digest (docs/OWNER-DIGEST.md).
--
-- Two admin-managed settings join the notification settings row (migration
-- 036), hot-swapped into the live notifier like the rest of it:
--
--   owner_digest_enabled   — send each task owner one email a day listing
--                            their scheduled tasks that did not work in the
--                            last 24 hours. Off by default: a defaulted ADD
--                            leaves every existing deployment as it was.
--   owner_digest_hour_utc  — the UTC hour (0-23) at or after which the day's
--                            digest goes out. 13 by default.
--
-- owner_digest_days records which UTC days' digests have been claimed, one
-- row per day, so the digest goes out at most once a day per deployment
-- across restarts and replicas: the sender claims the day with
-- INSERT ... ON CONFLICT DO NOTHING before it sends, and only the claim that
-- inserted the row sends. Rows older than 90 days are pruned on claim.
ALTER TABLE notify_settings ADD COLUMN owner_digest_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE notify_settings ADD COLUMN owner_digest_hour_utc INTEGER NOT NULL DEFAULT 13
    CHECK (owner_digest_hour_utc BETWEEN 0 AND 23);

CREATE TABLE IF NOT EXISTS owner_digest_days (
    day        DATE PRIMARY KEY,
    claimed_at BIGINT NOT NULL
);
