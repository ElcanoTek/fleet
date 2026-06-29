-- 035_add_task_sla_monitoring.up.sql — task SLA monitoring (#274).
--
-- Scheduled tasks have a hard process-kill ceiling (max_iterations at the
-- agent-core level) but no SLA concept: an operator cannot say "this task
-- should finish in 15 minutes; warn me at 22.5 and fail at 30". These columns
-- add a lightweight, OPTIONAL expected-duration field with warn/fail multipliers,
-- a one-shot breach flag set by the SLA monitor goroutine, and the actual run
-- duration populated on completion so the SLA report can compare expectation
-- vs. reality.
--
--   expected_duration_minutes — operator's typical-runtime expectation (NULL = no SLA)
--   sla_warn_multiplier       — elapsed >= expected * warn fires a WARN (default 1.5)
--   sla_fail_multiplier       — elapsed >= expected * fail fires a FAIL + breach (default 2.0)
--   sla_breached              — latched once the fail threshold is crossed (cleared on replay/re-run)
--   actual_duration_seconds   — completed_at - started_at, populated on terminal transition
--
-- expected_duration_minutes / actual_duration_seconds are NULLable so existing
-- rows backfill cleanly and tasks without an SLA are untouched. The two
-- multipliers are NOT NULL DEFAULT so a row with an expected duration always
-- has well-defined warn/fail thresholds even if the create request omitted
-- them. The partial index backs the SLA report query (last 7 days, grouped by
-- prompt) without bloating the index for the common non-SLAned rows.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS expected_duration_minutes INTEGER;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sla_warn_multiplier        REAL NOT NULL DEFAULT 1.5;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sla_fail_multiplier        REAL NOT NULL DEFAULT 2.0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sla_breached               BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS actual_duration_seconds    INTEGER;

CREATE INDEX IF NOT EXISTS idx_tasks_sla
    ON tasks (prompt, completed_at, sla_breached, actual_duration_seconds)
    WHERE expected_duration_minutes IS NOT NULL;
