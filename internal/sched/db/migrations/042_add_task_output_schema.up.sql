-- 042_add_task_output_schema.up.sql — structured output mode (#244).
--
-- output_schema is an optional draft-07 JSON Schema (nullable JSONB) a task
-- declares so its final answer is machine-readable. When set, the scheduled
-- driver augments the system prompt with the schema and validates the agent's
-- final text against it after the run. output_json holds the validated JSON
-- result (nullable JSONB) — NULL when no schema was declared or validation
-- failed. Both NULL on every existing row = free-form text mode (the prior
-- behaviour), so existing tasks are byte-for-byte unchanged.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS output_schema JSONB;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS output_json JSONB;
