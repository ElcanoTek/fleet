-- 035_add_task_sla_monitoring.down.sql — revert the SLA monitoring columns (#274).
DROP INDEX IF EXISTS idx_tasks_sla;
ALTER TABLE tasks DROP COLUMN IF EXISTS actual_duration_seconds;
ALTER TABLE tasks DROP COLUMN IF EXISTS sla_breached;
ALTER TABLE tasks DROP COLUMN IF EXISTS sla_fail_multiplier;
ALTER TABLE tasks DROP COLUMN IF EXISTS sla_warn_multiplier;
ALTER TABLE tasks DROP COLUMN IF EXISTS expected_duration_minutes;
