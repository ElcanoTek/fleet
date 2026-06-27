-- Per-task IANA timezone for evaluating cron recurrence in the task owner's
-- local time. Existing rows backfill to 'UTC', which exactly matches the prior
-- behaviour (schedule.Next() was always computed against the server-global
-- timezone, which defaults to UTC) — so no existing task silently shifts its
-- firing time.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'UTC';
