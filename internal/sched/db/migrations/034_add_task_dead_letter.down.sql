-- 034_add_task_dead_letter.down.sql — revert the dead-letter-queue columns (#253).
DROP INDEX IF EXISTS idx_tasks_dead_lettered_at;
ALTER TABLE tasks DROP COLUMN IF EXISTS dead_letter_attempts;
ALTER TABLE tasks DROP COLUMN IF EXISTS dead_letter_reason;
ALTER TABLE tasks DROP COLUMN IF EXISTS dead_lettered_at;
