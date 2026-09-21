-- 071_recurrence_unspawned_includes_dead_lettered.up.sql — ADR-0070.
--
-- A dead-lettered recurring occurrence now spawns its successor (or parks
-- after two consecutive dead-letters). The reconciliation sweep must be able
-- to see unsettled dead_lettered rows the same way it sees success/error, so
-- a crash between the quarantine commit and the post-commit spawn heals
-- instead of ending the schedule. Widen the partial index predicate to match
-- models.RecurrenceSpawnTaskStatuses. Existing dead_lettered recurring rows
-- stay unsettled on purpose: the first sweep after this lands repairs them.
DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error', 'dead_lettered');
