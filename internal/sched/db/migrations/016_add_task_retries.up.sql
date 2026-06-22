-- 016_add_task_retries.up.sql — whole-task retry/backoff at the scheduler boundary.
--
-- attempt_count tracks how many times a task has been re-queued after a clean,
-- transient failure; max_retries caps that (number of ADDITIONAL attempts beyond
-- the first — 0 = no retries, the prior behavior). NOT NULL DEFAULT 0 backfills
-- existing rows so behavior is unchanged unless a task opts in.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS max_retries INTEGER NOT NULL DEFAULT 0;
