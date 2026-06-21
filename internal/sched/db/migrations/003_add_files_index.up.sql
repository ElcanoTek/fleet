CREATE INDEX IF NOT EXISTS idx_tasks_files ON tasks USING GIN (files);
