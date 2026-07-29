# Per-attempt run log history

## The gap

`logs` keeps exactly one row per task — the latest transcript — and every
re-run of the **same task id** upserts over it. Recurring occurrences were
never affected (each occurrence is a new task row via the `TaskToCreate`
clone, so it gets its own `logs` row), but three paths legitimately re-run
one task id and used to destroy the prior attempt's transcript each time:

- a **retry** after a transient failure (`RetryPolicy`),
- an **ask-pause resume** (#510) — the answered re-run replaces the paused
  partial transcript,
- a **self-wake cycle** (`docs/SELF-WAKE.md`) — every wake re-runs the task.

A nightly task that misbehaved on Tuesday had no Tuesday transcript by
Thursday if Wednesday's retry overwrote it.

## What shipped

**Copy-on-overwrite into `run_logs`.** Inside the same transaction as the
`logs` upsert, the row the upsert would clobber is first copied verbatim into
`run_logs` (migration `058_add_run_log_history`). Verbatim means the archival
columns travel unchanged: a payload already archived by the log-archival
sweep (#272, gzip + optional AES-256-GCM) stays archived; a live plaintext
payload stays live. A task that only ever writes one transcript writes
nothing to `run_logs` — history costs storage only where an overwrite would
otherwise have destroyed a transcript.

**Bounded per task.** The same transaction trims each task's history to the
most recent 20 superseded transcripts (`runLogHistoryKeep`), oldest dropped
first — the cap can never be exceeded between sweeps.

**Pruned with the task.** Both retention paths (`CleanupOldRuns` #252 and
`DeleteOldHistory`) delete a task's `run_logs` rows in the same transaction
as its `logs` row, so history never outlives the task or dodges retention.

**Read surface.** Two endpoints behind exactly the `GET /logs/{task_id}`
gate (`PermissionViewLogs` + scoped-principal task visibility):

- `GET /logs/{task_id}/history` — metadata only (`id`, `superseded_at`),
  newest first; never drags payloads across the wire.
- `GET /logs/{task_id}/history/{entry_id}` — one superseded transcript in
  the same `LogSession` shape `GET /logs/{task_id}` returns. Entry lookups
  are task-scoped, so an id can never read another task's history.

**Web.** The task log modal grows an attempt picker ("Latest transcript" /
"Superseded <time>") that only renders when history exists.

## Deliberately deferred

- The log-archival sweep (#272) does not compress `run_logs` rows that were
  copied while still live; they are bounded by the per-task cap and pruned by
  retention, so the storage exposure is small. Archival of history rows can
  join #272's sweep later if it matters in practice.
- No CLI surface; the HTTP endpoints and the web picker are the read paths.
