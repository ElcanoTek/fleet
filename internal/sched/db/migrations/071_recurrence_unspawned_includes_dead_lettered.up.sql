-- 071_recurrence_unspawned_includes_dead_lettered.up.sql — ADR-0070.
--
-- A dead-lettered recurring occurrence now spawns its successor (or parks
-- after two consecutive dead-letters). The reconciliation sweep must be able
-- to see unsettled dead_lettered rows the same way it sees success/error, so
-- a crash between the quarantine commit and the post-commit spawn heals
-- instead of ending the schedule. Widen the partial index predicate to match
-- models.RecurrenceSpawnTaskStatuses.
--
-- Settle EXISTING dead_lettered rows FIRST. Before this change only
-- success/error ever claimed the spawn credit, so every organic DLQ row is
-- recurrence_spawned=FALSE — including rows whose lineage ALREADY continued
-- (a later success, a "run now", a live scheduled head). The consecutive-
-- dead-letter breaker does not catch that: the predecessor is often a
-- success. Leaving those rows unsettled would make the first
-- ReconcileRecurrences sweep insert a SECOND successor (a duplicate chain)
-- and resurrect long-dead chains. ADR-0070 therefore applies only to
-- dead-letters that happen AFTER this lands. Pre-upgrade parked chains
-- continue via replay exactly as today (replay re-arms because they have
-- no later occurrence in the chain). Non-recurring DLQ rows are settled
-- too; the flag is inert there (the sweep only selects recurrence <> '').
UPDATE tasks SET recurrence_spawned = TRUE
WHERE status = 'dead_lettered' AND NOT recurrence_spawned;

DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error', 'dead_lettered');
