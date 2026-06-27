-- 021_add_webhook_triggers.up.sql — external webhook triggers (#177).
--
-- A task may now be fired by an authenticated inbound HTTP event instead of (or
-- in addition to) the cron cadence. trigger_type distinguishes the two:
--   'cron'    — the existing behavior; the scheduler promotes it when due.
--   'webhook' — a TEMPLATE task that the cron engine never runs. It is stored
--               inert (status='scheduled', scheduled_for=NULL) and waits for a
--               POST /triggers/{slug}; each authenticated hit spawns a fresh
--               one-shot run cloned from the template with the rendered prompt.
-- Existing rows default to 'cron', preserving current behavior exactly.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS trigger_type TEXT NOT NULL DEFAULT 'cron';

-- task_triggers binds a URL-safe slug + HMAC-SHA256 secret to a template task.
-- The secret authenticates inbound webhook calls (constant-time compare); it is
-- never the admin API key, so external services (GitHub, Slack, CI) can call the
-- endpoint without ever seeing admin credentials. prompt_template is a Go
-- text/template rendered against the decoded JSON payload before the run spawns.
CREATE TABLE IF NOT EXISTS task_triggers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    slug            TEXT NOT NULL UNIQUE,
    secret          TEXT NOT NULL,
    prompt_template TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_triggers_task_id ON task_triggers(task_id);
