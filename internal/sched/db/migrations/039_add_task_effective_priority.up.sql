-- 039_add_task_effective_priority.up.sql — task priority queues (#230).
--
-- Flips the task-priority convention to "LOWER integer = HIGHER urgency"
-- (matching POSIX nice / ionice and most job schedulers), and splits the
-- concept into two columns:
--
--   priority           — the immutable value the submitter requested (0–100)
--   effective_priority — what the scheduler ACTUALLY orders the pending queue
--                        by; equal to priority at creation, lowered only by the
--                        anti-starvation sweep so a long-waiting task is
--                        eventually claimed without rewriting the request.
--
-- The pending claim path now ORDER BYs effective_priority ASC, created_at ASC.
-- Before this, ClaimNextPendingTask ordered priority DESC (higher = more urgent)
-- and an unset priority was the Go zero-value 0. The backfill below reverses the
-- old scale and maps the unset default to Normal (50). This is a best-effort
-- approximation, not a perfect order-preserving remap: a legacy row that
-- explicitly set a small non-zero priority (1–49) under the old DESC scale lands
-- at a less-urgent effective value than an unset (0→50) row, so their relative
-- order is not preserved. This only matters for tasks left PENDING across the
-- upgrade (terminal rows are never claimed), and in practice nearly all rows used
-- the unvalidated default of 0 — so the realistic impact is nil.
--
-- All statements run inside one transaction (DDL is transactional in Postgres),
-- so a failure leaves the schema untouched rather than half-migrated.

-- effective_priority defaults to Normal; adding a NOT NULL column with a
-- constant default is a metadata-only change on PG 11+ (no table rewrite).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS effective_priority INT NOT NULL DEFAULT 50;

-- Backfill existing rows from the OLD priority scale into the new convention:
--   old 0 (unset / Go zero-value)                  → 50 (Normal)
--   old non-zero (higher = more urgent under DESC) → 100 - priority, clamped to
--   [0,100] so a more-urgent old row becomes a lower (more-urgent) new value.
UPDATE tasks SET effective_priority = CASE
    WHEN priority = 0 THEN 50
    ELSE GREATEST(0, LEAST(100, 100 - priority))
END;

-- Clamp any legacy out-of-range submitted priority into [0,100] BEFORE adding
-- the CHECK, so the constraint cannot fail validating pre-existing rows
-- (priority was historically unvalidated).
UPDATE tasks SET priority = GREATEST(0, LEAST(100, priority))
    WHERE priority < 0 OR priority > 100;

-- Enforce the range on both columns going forward.
ALTER TABLE tasks ADD CONSTRAINT tasks_effective_priority_range
    CHECK (effective_priority BETWEEN 0 AND 100);
ALTER TABLE tasks ADD CONSTRAINT tasks_priority_range
    CHECK (priority BETWEEN 0 AND 100);

-- Partial index backing the hot claim query (status='pending', ordered by
-- effective_priority then created_at). Partial so it stays small — only pending
-- rows are ever claimed.
CREATE INDEX IF NOT EXISTS tasks_claim_idx
    ON tasks (effective_priority, created_at)
    WHERE status = 'pending';
