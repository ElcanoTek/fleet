-- Undoes 0210: the started_at index goes, the cascading task_id foreign key
-- comes back and the attribution columns go. Safe to run again.
--
-- NOT VALID: iterations of tasks deleted while 0210 was applied have no task
-- row, and are kept rather than deleted. The previous release joins
-- task_iterations to tasks, so it does not count them, and their attribution
-- is not recovered if 0210 is applied again (their task is gone). New rows
-- are checked and deleting a task deletes its iterations, as before 0210.
DROP INDEX IF EXISTS sched.idx_task_iterations_started_at;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_constraint
         WHERE conrelid = 'sched.task_iterations'::pg_catalog.regclass
           AND confrelid = 'sched.tasks'::pg_catalog.regclass
           AND contype = 'f'
    ) THEN
        ALTER TABLE sched.task_iterations
            ADD CONSTRAINT task_iterations_task_id_fkey
                FOREIGN KEY (task_id) REFERENCES sched.tasks (id) ON DELETE CASCADE NOT VALID;
    END IF;
END
$$;

ALTER TABLE sched.task_iterations
    DROP COLUMN IF EXISTS model,
    DROP COLUMN IF EXISTS created_by_key_id,
    DROP COLUMN IF EXISTS created_by;
