-- 065_add_recurrence_spawned.down.sql — drop the recurrence-spawn marker (#1116).
DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
ALTER TABLE tasks DROP COLUMN IF EXISTS recurrence_spawned;
