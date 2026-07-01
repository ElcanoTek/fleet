-- 049_add_task_pause.down.sql — drop the ask/notify pause columns (#510).
DROP INDEX IF EXISTS idx_tasks_paused;
ALTER TABLE tasks DROP COLUMN IF EXISTS pending_answer;
ALTER TABLE tasks DROP COLUMN IF EXISTS pending_question;
