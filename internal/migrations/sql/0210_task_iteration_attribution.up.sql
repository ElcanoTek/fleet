-- 0210_task_iteration_attribution: spend recorded for a task outlives the task.
--
-- sched.task_iterations is the task-side spend record: one row per run
-- iteration with its cost and tokens. The usage report (GET /admin/usage and
-- its CSV), the adoption report and the per-principal budgets aggregate it by
-- the task's creator, API key and model. Spend already incurred keeps counting
-- toward budgets and usage after the task is deleted — by a person, by
-- POST /tasks/cleanup or by the retention sweep (FLEET_RUN_LOG_RETENTION_DAYS)
-- — the way chat.turn_metrics outlives its conversation (chat migration 038).
--
-- So the row carries that attribution itself, copied from the task when the
-- iteration is first written (db.AddTaskIteration) and kept in step by the
-- admin owner reassignment (storage.SetTaskOwner, SetTaskLineageOwner), and
-- task_id stays as provenance without a foreign key. The readers no longer
-- join sched.tasks.
--
-- created_by is the account id, resolved to its email at read time as before
-- (a deleted account's spend shows under its id); created_by_key_id the
-- submitting key; model the task's pinned model ('' when it ran on the
-- deployment default, as before).
ALTER TABLE sched.task_iterations
    ADD COLUMN IF NOT EXISTS created_by        UUID,
    ADD COLUMN IF NOT EXISTS created_by_key_id TEXT,
    ADD COLUMN IF NOT EXISTS model             TEXT;

-- Every existing iteration belongs to an existing task (the foreign key held
-- until now), so this copies the attribution the readers grouped it by. A task
-- with none of the three set leaves its iterations' new columns NULL already,
-- so those rows are not rewritten.
UPDATE sched.task_iterations AS ti
   SET created_by        = t.created_by,
       created_by_key_id = t.created_by_key_id,
       model             = t.model
  FROM sched.tasks AS t
 WHERE t.id = ti.task_id
   AND (t.created_by IS NOT NULL OR t.created_by_key_id IS NOT NULL OR t.model IS NOT NULL);

-- Drop every foreign key from task_iterations to tasks, whatever its name
-- (legacy sched migration 023 created one with ON DELETE CASCADE under
-- Postgres's default name, task_iterations_task_id_fkey).
DO $$
DECLARE
    fk record;
BEGIN
    FOR fk IN
        SELECT conname FROM pg_catalog.pg_constraint
         WHERE conrelid = 'sched.task_iterations'::pg_catalog.regclass
           AND confrelid = 'sched.tasks'::pg_catalog.regclass
           AND contype = 'f'
    LOOP
        EXECUTE pg_catalog.format('ALTER TABLE sched.task_iterations DROP CONSTRAINT %I', fk.conname);
    END LOOP;
END
$$;

-- The rows are no longer removed with their task, so the table keeps every
-- run's spend. The usage report and every budget check on task create
-- aggregate a started_at window of it; this index keeps that a range scan.
CREATE INDEX IF NOT EXISTS idx_task_iterations_started_at
    ON sched.task_iterations (started_at);
