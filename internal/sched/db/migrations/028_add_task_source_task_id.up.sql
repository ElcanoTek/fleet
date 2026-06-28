-- Task re-run / clone lineage (#270): when a task is created via POST
-- /tasks/{id}/rerun or /clone, source_task_id records the task it was copied
-- from, so the UI can show a "re-run of <id>" breadcrumb and GET
-- /tasks?source_task_id=<id> can list a task's re-runs. NULL for original tasks.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS source_task_id TEXT;

CREATE INDEX IF NOT EXISTS idx_tasks_source_task_id ON tasks (source_task_id);
