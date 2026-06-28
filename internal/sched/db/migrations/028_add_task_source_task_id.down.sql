DROP INDEX IF EXISTS idx_tasks_source_task_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS source_task_id;
