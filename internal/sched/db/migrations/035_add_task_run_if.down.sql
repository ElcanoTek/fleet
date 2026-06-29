-- 035_add_task_run_if.down.sql — revert the conditional-execution columns (#269).
ALTER TABLE tasks DROP COLUMN IF EXISTS last_skip_reason;
ALTER TABLE tasks DROP COLUMN IF EXISTS last_skip_at;
ALTER TABLE tasks DROP COLUMN IF EXISTS skip_count;
ALTER TABLE tasks DROP COLUMN IF EXISTS run_if;
