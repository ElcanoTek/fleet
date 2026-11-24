-- Undoes 0213. The previous release neither reads nor writes the column, and
-- its lease recovery re-queues every expired lease as before. Safe to run
-- again.
ALTER TABLE sched.tasks DROP COLUMN IF EXISTS finished_run_id;
