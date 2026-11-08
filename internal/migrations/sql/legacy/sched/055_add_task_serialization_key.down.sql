-- 055_add_task_serialization_key.down.sql — drop the mutual-exclusion key (#709).
DROP INDEX IF EXISTS idx_tasks_serialization_active;
ALTER TABLE tasks DROP COLUMN IF EXISTS serialization_key;
