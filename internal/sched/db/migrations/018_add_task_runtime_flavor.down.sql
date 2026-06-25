-- Reverse 018_add_task_runtime_flavor.
ALTER TABLE tasks DROP COLUMN IF EXISTS runtime_flavor;
