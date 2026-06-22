-- Reverse 016_add_task_retries.
ALTER TABLE tasks DROP COLUMN IF EXISTS max_retries;
ALTER TABLE tasks DROP COLUMN IF EXISTS attempt_count;
