-- 071_recurrence_unspawned_includes_dead_lettered.down.sql — restore the
-- pre-ADR-0070 partial index (success/error only) and drop the park stamp.
--
-- The backfill that settled existing dead_lettered rows is NOT undone: those
-- credits were claimed in production under the old "DLQ never spawns" rule
-- (or the chain already continued some other way). Unsettling them on
-- rollback would re-expose the duplicate-chain hazard if this migration is
-- re-applied, and would not restore a spawn the old binary would never make.
-- Dropping recurrence_parked_at discards the park stamps with the column.
DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error');
ALTER TABLE tasks DROP COLUMN IF EXISTS recurrence_parked_at;
