-- 049_add_task_pause.up.sql — ask/notify + paused-awaiting-human run state (#510).
--
-- A scheduled agent can call the `ask` tool to pose a BLOCKING question; the run
-- ends (releasing the sandbox + lease — a paused task never holds a container),
-- the task parks in the new non-terminal 'paused_awaiting_input' status with the
-- question stored here, and a human answer re-queues it (status→pending) with
-- the answer carried in pending_answer. The next run injects the Q&A and clears
-- both columns. Nullable, no CHECK on status (it is a free TEXT column), written
-- by dedicated guarded UPDATEs (like error_analysis) and read in scanTask —
-- deliberately excluded from the task INSERT (a new task never starts paused).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pending_question TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pending_answer TEXT;

-- Backs the "list paused tasks awaiting input" queue (the enterprise
-- filterable-paused criterion).
CREATE INDEX IF NOT EXISTS idx_tasks_paused ON tasks (status) WHERE status = 'paused_awaiting_input';
