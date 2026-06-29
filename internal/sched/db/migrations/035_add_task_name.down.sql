-- Reverse of 035_add_task_name: drop the partial unique index and the name
-- column added for task-definition import/export (#238).
DROP INDEX IF EXISTS idx_tasks_name_unique;
ALTER TABLE tasks DROP COLUMN IF EXISTS name;
