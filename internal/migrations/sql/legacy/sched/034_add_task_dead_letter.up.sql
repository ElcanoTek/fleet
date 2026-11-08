-- 034_add_task_dead_letter.up.sql — dead-letter queue (#253).
--
-- After a scheduled task exhausts its retry budget (or fails with a
-- non-retryable error), the runner routes it to the new `dead_lettered` terminal
-- status instead of bare `error`, so operators can review and replay it. These
-- columns capture the quarantine context:
--
--   dead_lettered_at     — when the task entered the DLQ (NULL otherwise)
--   dead_letter_reason   — the final terminal-attempt failure message
--   dead_letter_attempts — total attempts made (attempt_count + 1) at quarantine
--
-- All NULLable / DEFAULT NULL so existing rows backfill cleanly and behavior is
-- unchanged for tasks that still retry/succeed. The `dead_lettered` status value
-- is enforced only in Go (TaskStatus is a free-text column, like the other
-- statuses), so no enum/check-constraint change is needed here. A partial index
-- on dead_lettered_at keeps the DLQ listing (status='dead_lettered', newest
-- first) cheap without bloating the index for the common non-quarantined rows.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS dead_lettered_at     TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS dead_letter_reason   TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS dead_letter_attempts INTEGER;

CREATE INDEX IF NOT EXISTS idx_tasks_dead_lettered_at
    ON tasks (dead_lettered_at DESC)
    WHERE dead_lettered_at IS NOT NULL;
