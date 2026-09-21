# Dead-lettered recurrences spawn a successor

## What shipped

A dead-lettered occurrence of a recurring task now spawns the next
occurrence the same way a success or error does: post-commit
`scheduleNextRecurrence`, spawn credit claimed, `previous_occurrence_id` +
`lineage_id` stamped, task memory carried. A one-off quarantine no longer
silently ends the schedule.

Two consecutive dead-lettered occurrences park the chain instead. The
breaker lives in `scheduleNextRecurrence`, so the post-commit path and
`ReconcileRecurrences` cannot disagree. Parked chains settle the spawn
credit (no forever re-evaluation) and continue when the operator replays.

`ReplayDeadLetteredTask` re-arms `recurrence_spawned` only when no successor
row exists. If the dead-letter already spawned, replay re-runs that
occurrence and cannot fork a second chain.

Cancel still ends the chain. What makes a run fail is unchanged.

Upgrade does **not** auto-resume chains parked before it. Migration 071
settles every existing `dead_lettered` row so the reconcile sweep cannot
fork a duplicate chain next to a lineage that already continued, and cannot
resurrect long-dead schedules. Those parked rows continue via replay, as
they did before.

See [ADR-0070](adr/0070-dead-lettered-recurrences-spawn-successor.md).
Production evidence: two daily jobs on 2026-09-19 (lineages `5089a219`,
`6a811210`) each dead-lettered once and never spawned again.

## What deviated

Crash-recovery quarantine (`db.RecoverExpiredLeases`) still writes
`dead_lettered` in bulk and does not call `DeadLetterTaskWithContext`;
rows quarantined **after** the upgrade are repaired by
`ReconcileRecurrences` after the grace window, which is the same repair
path a crashed post-commit spawn already used.

The first draft left existing DLQ rows unsettled so the sweep would
"repair" them. That is unsafe: organically dead-lettered rows all had
`recurrence_spawned=FALSE`, including lineages that already continued
(production lineage `ef49b641`: a dead-lettered occurrence, then two
successes and a live scheduled head). The consecutive-DLQ breaker does
not catch that (the predecessor is a success). Migration 071 therefore
backfill-settles existing `dead_lettered` rows; ADR-0070 applies only to
dead-letters after the upgrade.

## What was deferred

- A configurable consecutive-failure threshold. Two is hardcoded: one bad
  day continues, two is systemic.
- Spawning inside the lease-recovery UPDATE itself (would need RETURNING
  the quarantined rows). The sweep covers it.
- Out-of-band notification when a chain parks. The log line is the signal
  today.
