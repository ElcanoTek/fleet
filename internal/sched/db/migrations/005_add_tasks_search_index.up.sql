CREATE INDEX IF NOT EXISTS idx_tasks_prompt_search ON tasks USING GIN (to_tsvector('english', prompt));
