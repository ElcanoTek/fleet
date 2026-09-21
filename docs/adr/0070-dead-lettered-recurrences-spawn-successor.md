# ADR-0070: A dead-lettered recurring occurrence spawns its successor

- **Status:** Accepted
- **Date:** 2026-09-20
- **Deciders:** fleet maintainers

## Context

Dead-lettering (#253) is a terminal quarantine: retries exhausted, or a
non-retryable failure. Until this decision it deliberately did **not** spawn
the next occurrence of a recurring task. The quarantined row awaited
`ReplayDeadLetteredTask`, and the schedule resumed only if that replay's own
completion claimed the spawn credit. Cancel still ended the chain; that part
was right. Dead-letter was treated like cancel.

Production on 2026-09-19 showed the cost. Two daily dashboard-refresh jobs
on a production deployment were each dead-lettered once — one for a
transient provider error, one for an unresolved completion verification
after the write had already landed. No successor row was inserted. Nobody
noticed for days. A one-off bad day ended a daily job.

The same argument was already accepted for ask-pause expiry
(`storage.ExpirePausedTasks`, #1116) and stranded-wake expiry
(`ExpireStrandedWakeTasks`): an unattended pause on a recurring occurrence
must not silently kill the schedule. Dead-letter is another terminal
failure of a single occurrence. The schedule is not the occurrence.

Two consecutive dead-letters is a different signal: the job is broken, not
unlucky. Parking then is the right backstop, and replay remains how a
parked chain continues.

## Decision

When a recurring occurrence is dead-lettered (`runner.sendToDeadLetter` →
`storage.DeadLetterTaskWithContext`), spawn its successor exactly as a
success or error transition does: post-commit `scheduleNextRecurrence`, the
same idempotent spawn-credit contract, `previous_occurrence_id` and
`lineage_id` stamped, task memory carried.

If that occurrence's immediate predecessor (`previous_occurrence_id`) is
also `dead_lettered`, do **not** spawn. Park the chain: claim the spawn
credit (status-gated, so a concurrent replay cannot be clobbered) so
`ReconcileRecurrences` does not re-evaluate it forever, and log clearly
that replay continues the chain.
Two in a row is treated as systemic.

The breaker lives in **one** place — inside `scheduleNextRecurrence` (or a
helper it calls) — so both the post-commit path and the reconcile sweep
agree. `db.GetUnspawnedRecurringTasks` therefore selects `dead_lettered`
rows as well as `success`/`error`. Cancel still ends the chain.

`ReplayDeadLetteredTask` re-arms `recurrence_spawned` only when no later
recurrence occurrence exists in the same chain. A row pointing at this
one via `previous_occurrence_id` is definitive regardless of `created_at`.
`dead_lettered` is not cleanup-eligible, so a parent can outlive a pruned
success/error successor: a newer same-lineage recurring row is the
fallback, excluding clone-created chains (ancestry root has
`source_task_id` set). A breaker-parked chain with nothing newer re-arms
and continues on replay.

## Enforcement

- `storage.DeadLetterTaskWithContext` calls `scheduleNextRecurrence` after
  the quarantine commit.
- `storage.scheduleNextRecurrence` applies the consecutive-dead-letter
  breaker and settles without spawning when it trips.
- `db.GetUnspawnedRecurringTasks` selects `models.RecurrenceSpawnTaskStatuses`
  (`success`, `error`, `dead_lettered`); `db.recurrenceSpawnedInsertValue`
  settles born-terminal rows in that same set.
- `storage.ReplayDeadLetteredTask` re-arms the spawn credit only when no
  later recurrence occurrence exists in the chain (direct pointer, or a
  newer same-lineage row that still recurs).
- `Storage.AddTaskWithContext` / `db.AddTaskTx` settle `recurrence_spawned`
  when the write lands in `RecurrenceSpawnTaskStatuses`, so a
  `--replace-status` / `--overwrite` upsert over a live recurring row
  cannot leave restored history unclaimed.
- The DLQ-breaker settle requires `status = dead_lettered` so it cannot
  clobber a replay that already committed.
- Migration 071 backfill-settles existing `dead_lettered` rows before it
  widens the unspawned-recurrence index, so the sweep cannot fork a
  pre-upgrade lineage.
- Tests: `internal/sched/storage/deadletter_recurrence_test.go`,
  `internal/runner/deadletter_recurrence_test.go`,
  `TestReconcileRecurrencesDoesNotRespawnBackfilledDeadLetter`, and the
  existing reconcile/DLQ tests updated for the new contract.

## Consequences

- A single dead-letter no longer silently ends a recurring schedule. This
  applies to dead-letters **after** the upgrade. Migration 071 backfill-settles
  every existing `dead_lettered` row (`recurrence_spawned = TRUE`) so the
  first `ReconcileRecurrences` sweep cannot insert a duplicate successor for
  a lineage that already continued (a later success, a "run now", a live
  scheduled head) or resurrect a chain parked weeks ago. Chains parked
  **before** the upgrade are not auto-resumed; they continue via replay
  exactly as today (replay re-arms because they have no later occurrence
  in the chain).
- Two consecutive dead-letters still park the chain. Operators replay to
  continue; that is unchanged for the parked case.
- Replaying a dead-lettered occurrence that already has a successor re-runs
  **that** occurrence and does not mint a parallel chain.
- Crash-recovery quarantine (`db.RecoverExpiredLeases`) writes `dead_lettered`
  in bulk without calling `DeadLetterTaskWithContext`. Those rows are
  repaired by the same reconcile sweep (after the grace window), not by a
  second spawn path in the recovery UPDATE.
- What makes a run fail is unchanged. This only changes what happens to the
  schedule after a dead-letter.

## Alternatives considered

- **Keep parking on every dead-letter; page harder.** The production miss
  was "nobody looked," not "the DLQ listing was empty." Paging does not
  restore the missed days.
- **Always spawn, no breaker.** A permanently broken job would then produce
  a new dead-lettered row every tick. Two in a row is the smallest systemic
  signal that still lets a one-off bad day through.
- **Spawn from the runner, not storage.** That would fork a second opinion
  from `ReconcileRecurrences` and from any other `DeadLetterTaskWithContext`
  caller. The spawn belongs next to the other terminal transitions.
