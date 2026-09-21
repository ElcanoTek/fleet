-- 071_recurrence_unspawned_includes_dead_lettered.up.sql — ADR-0070.
--
-- A dead-lettered recurring occurrence now spawns its successor (or parks
-- after two consecutive dead-letters). The reconciliation sweep must be able
-- to see unsettled dead_lettered rows the same way it sees success/error, so
-- a crash between the quarantine commit and the post-commit spawn heals
-- instead of ending the schedule. Widen the partial index predicate to match
-- models.RecurrenceSpawnTaskStatuses.
--
-- recurrence_parked_at is the durable "this chain is parked" stamp. Replay
-- re-arms iff the row is parked OR the spawn credit is still unclaimed;
-- otherwise the DLQ path already spawned (or the row is settled history)
-- and replay must not fork a second chain. Folded into this unshipped
-- migration rather than 072: same ADR, not yet applied anywhere that is
-- not a throwaway test database.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS recurrence_parked_at TIMESTAMPTZ;

-- Settle EXISTING dead_lettered rows FIRST. Before this change only
-- success/error ever claimed the spawn credit, so every organic DLQ row is
-- recurrence_spawned=FALSE — including rows whose lineage ALREADY continued
-- (a later success, a "run now", a live scheduled head). The consecutive-
-- dead-letter breaker does not catch that: the predecessor is often a
-- success. Leaving those rows unsettled would make the first
-- ReconcileRecurrences sweep insert a SECOND successor (a duplicate chain)
-- and resurrect long-dead chains. ADR-0070 therefore applies only to
-- dead-letters that happen AFTER this lands. Non-recurring DLQ rows are
-- settled too; the flag is inert there (the sweep only selects recurrence <> '').
UPDATE tasks SET recurrence_spawned = TRUE
WHERE status = 'dead_lettered' AND NOT recurrence_spawned;

-- One-time best effort at upgrade: stamp park on recurring DLQ rows that
-- have no successor pointer. This is the ONLY place previous_occurrence_id
-- is consulted for park/replay. Acceptable at migration time because it
-- runs once against the live table; a live successor (or a later
-- occurrence that still points here) means the chain already continued,
-- so we must NOT park those. Rows with no pointer are treated as parked
-- so replay can continue them, matching pre-upgrade "replay is how a
-- parked chain continues".
UPDATE tasks SET recurrence_parked_at = COALESCE(dead_lettered_at, completed_at, now())
WHERE status = 'dead_lettered'
  AND recurrence IS NOT NULL AND recurrence <> ''
  AND recurrence_parked_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM tasks s WHERE s.previous_occurrence_id = tasks.id::text
  );

DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error', 'dead_lettered');
