-- Track which scoped API key submitted a task, so the completion path can
-- attribute its LLM cost back to that key for per-key spending caps. Nullable:
-- tasks created by admin/user auth (not an API key) leave it NULL, as do all
-- existing rows.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS created_by_key_id TEXT;
