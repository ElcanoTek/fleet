-- Down migration for 045_add_task_artifacts (#204).
ALTER TABLE tasks DROP COLUMN IF EXISTS artifacts;
