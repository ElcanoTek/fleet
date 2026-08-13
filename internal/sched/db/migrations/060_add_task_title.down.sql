-- 060_add_task_title.down.sql — drop the operator-facing task title.
ALTER TABLE tasks DROP COLUMN IF EXISTS title;
