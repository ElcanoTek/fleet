-- 074_add_task_run_outcome.up.sql — how a terminal run ended, beyond its status,
-- and the runner's infrastructure re-runs of one occurrence.
--
-- run_outcome / run_outcome_detail: a scheduled refresh that correctly decided
-- not to publish (its declared completion clause, EXECUTION REQUIREMENTS
-- completion.blocked_when, recorded "blocked", "failed", "source_unreachable",
-- ...) finished as plain success, so the Operations Center showed green for
-- days while a dashboard stopped updating. 'blocked' marks such a success;
-- 'connector_unavailable' marks a dead-letter caused by a declared connector
-- that failed to connect, which the recurrence park breaker does not count.
-- Written with the terminal transition, cleared by replay. NULL = an ordinary
-- run (and every run before this column).
--
-- infra_retry_count: how many times the runner re-ran this occurrence because
-- a declared connector was unavailable (a DNS blip, a vendor's "temporarily
-- unavailable"). Counted apart from attempt_count so it never spends the
-- task's own max_retries. 0 for every existing row.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS run_outcome TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS run_outcome_detail TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS infra_retry_count INTEGER NOT NULL DEFAULT 0;
