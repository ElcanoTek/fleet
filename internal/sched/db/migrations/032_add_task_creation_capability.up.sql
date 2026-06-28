-- Scheduled-agent task creation (#277): a built-in create_task tool lets a
-- SCHEDULED run of an opted-in task enqueue follow-up tasks. The capability is
-- gated by two per-task bits (default false, so existing tasks are unchanged and
-- cannot self-schedule), plus a lineage column for audit.
--
--   * allow_task_creation           — authority gate; the create_task tool is
--                                      registered for a scheduled run ONLY when
--                                      this is true. Default false = no privilege
--                                      escalation, no self-scheduling.
--   * allow_recurring_task_creation — stricter, separately-toggled bit that
--                                      additionally permits a spawned task to
--                                      carry a cron recurrence. Default false.
--   * created_by_task_id            — lineage: the task whose run spawned this
--                                      one (NULL for tasks not spawned by a task).
--                                      Self-referential FK, like source_task_id
--                                      (#270/migration 028) but distinct semantics.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS allow_task_creation BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS allow_recurring_task_creation BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS created_by_task_id TEXT;

CREATE INDEX IF NOT EXISTS idx_tasks_created_by_task_id ON tasks (created_by_task_id);
