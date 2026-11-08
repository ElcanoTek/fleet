-- 064_add_task_paused_at.down.sql — drop the pause-instant column (#1116).
ALTER TABLE tasks DROP COLUMN IF EXISTS paused_at;
