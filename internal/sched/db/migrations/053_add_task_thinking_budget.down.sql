-- 053_add_task_thinking_budget.down.sql — drop the per-task thinking override (#220).
ALTER TABLE tasks DROP COLUMN IF EXISTS thinking_budget_tokens;
