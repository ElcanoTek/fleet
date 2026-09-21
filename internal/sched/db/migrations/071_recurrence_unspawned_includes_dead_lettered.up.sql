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

-- One-time best effort at upgrade: stamp park on recurring DLQ rows whose
-- chain shows NO sign of having continued. This is the ONLY place chain
-- continuation is inferred from other rows (the runtime uses the stamp
-- alone). Two signals, either one means "already continued, do not park":
--   (a) a row still points here through previous_occurrence_id (the direct
--       successor survived retention), or
--   (b) a LATER recurring occurrence exists in the same lineage — the
--       durable signal when the immediate successor was pruned by
--       CleanupOldRuns / DeleteOldHistory (retention removes OLD rows, so a
--       chain that went on always has a newer row). Clones share lineage_id
--       and are deliberately counted too: the failure mode of NOT parking is
--       a stale chain an operator re-creates by hand, while parking a chain
--       that already continued lets a replay fork duplicates and repeat
--       external side effects — the conservative side is not to park.
-- Rows with neither signal are parked so `dlq replay` can continue them,
-- matching the pre-upgrade "replay is how a parked chain continues".
--
-- Known residual, deliberately accepted: history from before migration 069
-- carries its own id as lineage_id (069's backfill) and no predecessor
-- pointer (068 added the column later), which is also the shape of every
-- genuine chain root created since. For such a pre-069 dead-letter whose
-- immediate successor was later pruned by retention while descendants live
-- on, neither signal fires and the row IS stamped parked; a replay of it
-- would re-arm the credit and could spawn next to the surviving chain. The
-- migration has no discriminator for that case (schema_migrations records no
-- apply time), and refusing to park the whole root shape would instead break
-- replay for every post-069 first-run dead-letter. The residual is bounded:
-- it needs a pre-069 row, a successor old enough to have been pruned, AND an
-- explicit operator replay of that old occurrence.
UPDATE tasks SET recurrence_parked_at = COALESCE(dead_lettered_at, completed_at, now())
WHERE status = 'dead_lettered'
  AND recurrence IS NOT NULL AND recurrence <> ''
  AND recurrence_parked_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM tasks s WHERE s.previous_occurrence_id = tasks.id::text
  )
  AND NOT EXISTS (
      SELECT 1 FROM tasks l
      WHERE tasks.lineage_id IS NOT NULL
        AND l.lineage_id = tasks.lineage_id
        AND l.id <> tasks.id
        AND l.created_at > tasks.created_at
        AND l.recurrence IS NOT NULL AND l.recurrence <> ''
  );

DROP INDEX IF EXISTS idx_tasks_recurrence_unspawned;
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error', 'dead_lettered');
