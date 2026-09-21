# Dead-lettered recurrences spawn a successor

## What shipped

A dead-lettered occurrence of a recurring task now spawns the next
occurrence the same way a success or error does: post-commit
`scheduleNextRecurrence`, spawn credit claimed, `previous_occurrence_id` +
`lineage_id` stamped, task memory carried. A one-off quarantine no longer
silently ends the schedule.

Two consecutive dead-lettered occurrences park the chain instead. The
breaker lives in `scheduleNextRecurrence`, so the post-commit path and
`ReconcileRecurrences` cannot disagree. Parking stamps
`recurrence_parked_at` in the same transaction that claims the spawn
credit (no successor is inserted). Replay re-arms iff that stamp is set
or the spawn credit is still unclaimed, and clears the stamp. If the
DLQ path already spawned, replay keeps the credit claimed and cannot
fork a second chain.

Cancel still ends the chain. What makes a run fail is unchanged.

Upgrade does **not** auto-resume chains parked before it. Migration 071
settles every existing `dead_lettered` row so the reconcile sweep cannot
fork a duplicate chain next to a lineage that already continued, and cannot
resurrect long-dead schedules. Recurring DLQ rows whose chain shows no
sign of having continued — no row points at them through
`previous_occurrence_id` and no later recurring occurrence exists in their
lineage — also get `recurrence_parked_at` (one-time best effort; that is
the only time continuation is inferred from other rows, and it errs toward
NOT parking, because a wrongly parked row lets a replay fork duplicates
while a wrongly unparked one is only a stale chain to re-create). Rows in
the pre-migration-069 shape (no predecessor pointer, lineage equal to their
own id) are never stamped, because for that history neither signal can prove
the chain did not continue. Stamped rows continue via replay.

See [ADR-0070](adr/0070-dead-lettered-recurrences-spawn-successor.md).
Production evidence: two daily dashboard-refresh jobs on a production
deployment, 2026-09-19 — one dead-lettered for a transient provider error,
one for an unresolved completion verification after the write had already
landed — each never spawned again.

## What deviated

Crash-recovery quarantine (`db.RecoverExpiredLeases`) still writes
`dead_lettered` in bulk and does not call `DeadLetterTaskWithContext`;
rows quarantined **after** the upgrade are repaired by
`ReconcileRecurrences` after the grace window, which is the same repair
path a crashed post-commit spawn already used.

The first draft left existing DLQ rows unsettled so the sweep would
"repair" them. That is unsafe: organically dead-lettered rows all had
`recurrence_spawned=FALSE`, including lineages that already continued
(a dead-lettered occurrence, then later successes and a live scheduled
head). The consecutive-DLQ breaker does not catch that (the predecessor
is a success). Migration 071 therefore backfill-settles existing
`dead_lettered` rows; ADR-0070 applies only to dead-letters after the
upgrade.

## What was deferred

- A configurable consecutive-failure threshold. Two is hardcoded: one bad
  day continues, two is systemic.
- Spawning inside the lease-recovery UPDATE itself (would need RETURNING
  the quarantined rows). The sweep covers it.
- Out-of-band notification when a chain parks. The log line is the signal
  today.
