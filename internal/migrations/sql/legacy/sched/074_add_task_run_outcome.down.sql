-- 074_add_task_run_outcome.down.sql — drop the run outcome and the infra re-run count.
ALTER TABLE tasks DROP COLUMN IF EXISTS infra_retry_count;
ALTER TABLE tasks DROP COLUMN IF EXISTS run_outcome_detail;
ALTER TABLE tasks DROP COLUMN IF EXISTS run_outcome;
