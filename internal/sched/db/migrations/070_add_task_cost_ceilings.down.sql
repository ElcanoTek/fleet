-- 070_add_task_cost_ceilings.down.sql — drop the per-task run ceilings.
ALTER TABLE tasks DROP COLUMN IF EXISTS max_total_tokens;
ALTER TABLE tasks DROP COLUMN IF EXISTS max_cost_usd;
