-- Task documentation (#281): an optional, free-form Markdown description field
-- so operators can document why a task exists, what it costs, its side effects,
-- and the runbook if it fails. NULL = no description set. This is per-task
-- operator documentation, distinct from the shared agent-notes wiki (migration
-- 015), and is NOT injected into agent prompts.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS description TEXT;
