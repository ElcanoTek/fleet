-- 070_add_task_cost_ceilings.up.sql — per-task run ceilings (#1533).
--
-- The per-run cost and uncached-token ceilings were host-global
-- (FLEET_MAX_COST_USD / FLEET_MAX_TOTAL_TOKENS, or the admin settings). A job
-- that legitimately costs more than its neighbours could only be run by
-- raising the ceiling for every task on the box, or by changing its model.
--
-- max_cost_usd / max_total_tokens are DEFINITION fields (carried by the
-- TaskToCreate clone recipe, editable, exported): NULL = inherit the
-- deployment ceiling; a value replaces it for this task's runs through the
-- same checkCeilings path a spawned child's sliced budget already uses.
-- A value above the deployment ceiling needs the same authority as editing
-- the deployment ceiling (admin); anyone may lower a task's own.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS max_cost_usd DOUBLE PRECISION;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS max_total_tokens INTEGER;
