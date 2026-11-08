-- 040_add_task_sandbox_limits.down.sql — revert per-task sandbox limits (#205).
ALTER TABLE tasks DROP COLUMN IF EXISTS sandbox_limits;
