-- 040_add_task_sandbox_limits.up.sql — per-task sandbox resource limits (#205).
--
-- sandbox_limits is an optional per-task override of the global FLEET_SANDBOX_*
-- cgroup ceilings (memory / cpus / pids). NULL = use the global defaults (the
-- existing behaviour), so every existing row is untouched. Stored as JSONB:
--   {"memory_mb": 2048, "cpus": 2.0, "pids": 512}
-- with each field optional (a zero/absent field falls back to the global ceiling).
-- The scheduled runner applies it when it cold-starts the task's container — a
-- TIGHTENING of the mandatory sandbox, never a way to escape it.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS sandbox_limits JSONB;
