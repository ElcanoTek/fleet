-- 035_add_task_run_if.up.sql — conditional task execution (#269).
--
-- A scheduled task may carry an optional pre-run shell gate (`run_if`) that
-- the scheduler evaluates on the host before promoting a due task. When the
-- check fails the occurrence is skipped: status stays `scheduled`,
-- scheduled_for advances to the next cron tick, and the skip telemetry below
-- is recorded. nil run_if = the legacy unconditional promotion path.
--
--   run_if             — JSONB RunIf struct (command, exit_code_is,
--                        timeout_seconds, on_error); NULL = no gate
--   skip_count         — how many times this task's gate has skipped it
--   last_skip_at       — when the most recent skip happened
--   last_skip_reason   — the human-readable skip reason (exit code + stderr,
--                        or "check timed out")
--
-- All NULLable / DEFAULT NULL so existing rows backfill cleanly and behavior is
-- unchanged for tasks without a gate. run_if is JSONB so the struct can evolve
-- without additional migrations. skip_count defaults to 0 (NOT NULL) so the
-- common never-skipped row reads 0 without a nullable scan.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS run_if           JSONB;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS skip_count       INTEGER      NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_skip_at     TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_skip_reason TEXT;
