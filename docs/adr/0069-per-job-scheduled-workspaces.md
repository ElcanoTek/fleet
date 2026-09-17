# ADR-0069: A scheduled job works in its own directory under the workspace root

Status: accepted; refines #180 (git worktree isolation) and #287 (`workspace_path`).

## Context

A scheduled task without a `worktree_config` ran in the shared workspace root:
one directory for every job and every client on the host. Reklaim's daily
health scan (runs `41c45dc0`, `6bd0c212`, `f312eeb6`, 2026-09-17) shows the
cost. The directory listing had ~80 root entries, including other clients'
report downloads and UUID-named subdirectories from unrelated runs. Later runs
found and executed scripts an earlier run had left. The end-of-run audit read a
`render_email.log` written by a *different* job — naming recipients this task
never had — and a `health_summary.json` whose feed maxima did not match this
run's retrievals, and aborted a run whose own work was sound. Cross-client
leftovers in one directory are also a data-isolation concern on any host that
serves more than one client.

Git worktree isolation exists, but it is a full checkout per occurrence and is
opt-in per task; it is the wrong tool for the ordinary report job.

## Decision

Every task carries a **lineage id**: the key all runs of one job share. A task
created fresh is its own lineage (`lineage_id = id`); the `TaskToCreate` clone
recipe carries it to recurrence occurrences, re-runs and clones. It is
persisted (migration 069, written on every definition write so it can never
drift to NULL), never exported (a re-imported definition starts its own lineage
on the target), and not settable by clients.

A non-worktree scheduled run works in `<workspace-root>/tasks/<lineage_id>/`,
created on first use. The directory sits under the root the sandbox already
bind-mounts, so nothing changes about mounts; the supporting-doc symlinks are
seeded into it exactly as they were into the root; the run's `workspace_path`,
file browser and `publish_artifact` scope follow it. The per-run MCP
directories (`mcp-runs/`) and the reserved `${FLEET_WORKSPACE_ROOT}` token are
unaffected: the per-job directory is under the root, so a connector that
allowlists the root still reads a report written there.

`FLEET_SCHEDULED_SHARED_WORKSPACE=1` restores the shared root for every job.

## Consequences

- One job's leftovers never feed another job's reasoning or audit, and a job
  still reuses its own previous downloads across occurrences.
- The first run of an existing job after this lands starts in an empty
  directory and re-fetches what it needs; from then on its directory
  accumulates as the root used to.
- Cross-run artifact retention for non-worktree runs stays disabled (the
  reason it was disabled — a fixed path replaced by another live run — now
  applies only within one lineage, but a "Run now" can still overlap a
  scheduled occurrence of the same job).
- Two jobs that deliberately shared files through the root must now name the
  path explicitly; the root is no longer a working directory.
- Walking `source_task_id` / `previous_occurrence_id` chains was rejected as
  the lineage source: both are one-step pointers, the chain grows by one row
  per occurrence, and retention deletes old rows, which would silently rename
  a job's directory.
