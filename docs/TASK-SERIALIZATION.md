# Task serialization — opaque `serialization_key` mutual exclusion (#709)

Two concurrent runs doing read-modify-write against the same external state
(e.g. the same client's records) interleave, and the last writer silently wins.
The legacy scheduler closed this with an opaque per-task key (moc#442/#448);
this is the fleet port of that proven design.

## Contract

- `TaskCreate` accepts an optional, opaque `serialization_key` (string).
  Fleet guarantees **at most one task per key is active at a time** — active
  means `leased`, `running`, or `analyzing`.
- A pending task whose key matches an active task is **skipped at claim time**:
  it stays queued and is retried on a later claim pass. It is never failed,
  cancelled, or reordered — just deferred.
- Fleet **never interprets the key**. Its meaning is owned by the intake
  caller (coupling doctrine: intake apps send e.g. `client:<id>`; fleet must
  not accrete behavior around key contents).
- Absent / empty / whitespace-only keys are normalized to NULL = unserialized:
  byte-identical behavior for every existing task and old caller. Additive
  field; no lockstep deploys.
- Recurring tasks propagate the key to each next occurrence (it is part of the
  `TaskToCreate` clone recipe), so successive occurrences stay mutually
  exclusive with same-key tasks. Re-run/clone and the task-definition
  export/import (#238) carry it too. The key is immutable after creation (the
  task-edit path does not touch it).
- `paused_awaiting_input` does **not** hold the key: a paused run has stopped
  executing and its resume re-queues the task as pending, which re-passes the
  claim gate before it can run again.

## Where the guarantee lives

The gate sits inside `db.ClaimNextPendingTask` — the **only** pending→active
transition in fleet (retry/recovery/resume paths all re-queue to `pending`, so
every path back to execution re-passes it):

1. The candidate SELECT filters out pending tasks whose key is visibly held by
   an active task (`NOT EXISTS` over the active statuses, served by a partial
   index). This is best-effort visibility so a blocked task never consumes the
   LIMIT-1 candidate slot and starves the queue behind it.
2. A selected candidate that carries a key is re-checked under
   `pg_advisory_xact_lock(hashtext(key))` inside the claim transaction. The
   lock serializes concurrent same-key claims, so two claimers can never both
   pass the existence check; the loser declines (the task stays pending) and
   the lock auto-releases at commit/rollback. This locked re-check — not the
   visibility filter — is the correctness guarantee.

`hashtext` collisions across distinct keys are possible and harmless: they can
only make two unrelated claims briefly serialize, never interleave.

Schema: sched migration 055 (`tasks.serialization_key TEXT NULL` + partial
index over active statuses).

## Migration

The v1→fleet migration bundle carries `serialization_key` on `sched.tasks[]`
(#712), so a live recurring task keeps its serialization guarantee across the
cutover. See `docs/LEGACY-IMPORT.md`.

## Honest scope

- Single-process scheduler, but the gate is DB-backed (advisory lock), so it
  stays correct if multiple fleet processes ever share one sched DB.
- No fairness ordering among same-key waiters beyond the normal
  effective-priority/created_at claim order.
- No per-key queue-depth metrics or UI surfacing yet.
