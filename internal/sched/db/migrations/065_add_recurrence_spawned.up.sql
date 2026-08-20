-- 065_add_recurrence_spawned.up.sql — recurrence-spawn settlement marker (#1116).
--
-- A recurring task's next occurrence is spawned AFTER the completing
-- occurrence's terminal transaction commits (storage.scheduleNextRecurrence).
-- Any failure in that window — a transient AddTask DB error, or a crash between
-- the commit and the spawn — previously only logged, and the schedule was dead
-- forever: no status marked the chain broken and no sweep detected it.
--
-- recurrence_spawned is the settlement flag that makes the spawn idempotent and
-- repairable. TRUE means this terminal occurrence's successor question is
-- SETTLED: the next occurrence was inserted, or the chain legitimately ended
-- (recurrence_until passed / run budget exhausted / unparseable definition).
-- The spawner flips it in the SAME transaction that inserts the successor,
-- guarded by "AND NOT recurrence_spawned", so exactly one spawner can ever win
-- and a duplicate successor is structurally impossible. The scheduler tick's
-- reconciliation sweep re-drives any terminal recurring row still FALSE, so a
-- transient spawn failure now heals within a tick instead of ending the chain.
--
-- INTERNAL bookkeeping only: not read into models.Task (no reader needs it),
-- excluded from taskColumns/scanTask, the upsert clauses, UpdateTaskTx, the
-- TaskToCreate clone recipe, and the export record. Written by the guarded
-- spawn/settle statements in storage, re-armed (FALSE) by DLQ replay, and
-- derived at INSERT (recurrenceSpawnedInsertValue): a row born success/error —
-- restored history from `fleet import` — lands settled, so a restore can never
-- be mistaken for a fleet of lost spawns; every other status starts FALSE.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS recurrence_spawned BOOLEAN NOT NULL DEFAULT FALSE;

-- Settle every recurring row already in a spawn-bearing terminal state
-- (success/error): their spawn (or its loss) happened under the old
-- post-commit regime, and re-driving historical rows would duplicate
-- successors for every chain that DID spawn — and mass-resurrect long-dead
-- ones. Only completions after this migration participate in reconciliation.
-- Deliberately NOT settled: cancelled rows (never spawn; the sweep never
-- selects them, so the flag is inert) and dead_lettered rows (quarantine
-- parks the chain WITHOUT spawning — an unsettled flag is what lets a DLQ
-- replay's eventual completion claim the spawn credit and continue the chain).
-- Non-recurring rows are skipped outright: the flag is only ever consulted
-- where recurrence is set, and an unfiltered UPDATE would rewrite/WAL-log
-- every terminal row in the table for zero gain.
UPDATE tasks SET recurrence_spawned = TRUE
WHERE status IN ('success', 'error')
  AND recurrence IS NOT NULL AND recurrence <> '';

-- Backs the reconciliation sweep: terminal recurring rows whose spawn is still
-- unsettled. Near-empty in steady state (the normal post-commit spawn settles
-- the row within milliseconds), so the index costs almost nothing and keeps the
-- 30s-tick sweep a cheap indexed probe.
CREATE INDEX IF NOT EXISTS idx_tasks_recurrence_unspawned
    ON tasks (completed_at)
    WHERE recurrence IS NOT NULL
    AND NOT recurrence_spawned
    AND status IN ('success', 'error');
