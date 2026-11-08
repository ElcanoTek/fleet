-- 039_add_task_effective_priority.down.sql — revert task priority queues (#230).
-- The submitted-priority clamp is lossy (legacy out-of-range values cannot be
-- restored) and the runtime claim ordering lives in Go, not the schema; this
-- only removes the columns/constraints/index this migration added.
DROP INDEX IF EXISTS tasks_claim_idx;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_priority_range;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_effective_priority_range;
ALTER TABLE tasks DROP COLUMN IF EXISTS effective_priority;
