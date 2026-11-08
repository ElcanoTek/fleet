-- 053_add_task_thinking_budget.up.sql — per-task extended-thinking override (#220).
--
-- Scheduled runs previously honored only the GLOBAL thinking budget
-- (FLEET_DEFAULT_THINKING_BUDGET_TOKENS). This nullable column is the per-task
-- override: NULL = inherit the global default (unchanged behavior), 0 = force
-- thinking OFF for this task, N (>0) = this task's own budget (clamped to the
-- provider bounds at run time). Mirrors the nullable-int shape of
-- expected_duration_minutes (#274).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS thinking_budget_tokens INTEGER;
