-- 071_recurrence_unspawned_includes_dead_lettered.down.sql — restore the
-- pre-ADR-0070 partial index (success/error only).
DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error');
