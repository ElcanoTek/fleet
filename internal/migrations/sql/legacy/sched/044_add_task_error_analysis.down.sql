-- Down migration for 044_add_task_error_analysis (#317).
ALTER TABLE tasks DROP COLUMN IF EXISTS error_analysis;
