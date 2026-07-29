-- 058_add_run_log_history.down.sql — drop the per-attempt run log history.
DROP INDEX IF EXISTS idx_run_logs_task_superseded;
DROP TABLE IF EXISTS run_logs;
