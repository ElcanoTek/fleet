DROP INDEX IF EXISTS idx_tasks_created_by_task_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS created_by_task_id;
ALTER TABLE tasks DROP COLUMN IF EXISTS allow_recurring_task_creation;
ALTER TABLE tasks DROP COLUMN IF EXISTS allow_task_creation;
