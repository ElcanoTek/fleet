# ADR-0042: Run-log transcripts are creator-scoped; fleet-wide reads need an explicit grant

- **Status:** Accepted
- **Date:** 2026-08-14
- **Deciders:** fleet maintainers

## Context

Every run-log read — `GET /logs/{task_id}`, its per-attempt history siblings
(`/history`, `/history/{entry_id}`), and the live SSE stream
`GET /tasks/{task_id}/stream` — authorized on the `view_logs` permission
**alone**, with no scoping to the requesting principal's own tasks (issue #980;
the check dates to `a4be3d26`, 2026-06-30).

A run log is the most sensitive artifact a task produces. It carries the verbatim
tool traffic: connector query results, whatever PII the agent handled, and the
run's cost data. Under the old gate, *any* principal holding `view_logs` could
read *every* transcript on the box. That includes a `fleet_task_*` key minted for
one automation and every `fleet_readonly_*` key, both of which get `view_logs`
from their key type — and task ids are enumerable through `GET /tasks`, which the
same key may call. One leaked or over-issued CI key therefore exfiltrated every
client's transcripts with a single GET per task id. On a multi-client deployment
that is a cross-tenant read.

The inconsistency that marked it as unintended: the same principal was already
refused that task's **workspace files**, which are creator-private via
`taskWorkspaceOwned` (#287), and its **artifact manifest**, deliberately gated the
same way. The platform drew the per-task boundary for the *less* sensitive
artifact class and not for the more sensitive one.

A scope check was added to these routes after the audit, but it was a no-op:
`taskVisibleToScopes` returns `true` unconditionally (node-name scopes stopped
constraining anything when tasks lost their node target), and an *unscoped*
principal — which every typed key is unless someone set node patterns — skipped it
entirely.

## Decision

**`view_logs` admits a principal to the run-log surface; it does not decide which
tasks.** Reading a transcript additionally requires either

1. **ownership** — the task was created by this principal (the creating user, or
   the creating API key: `CreatedBy` / `CreatedByKeyID`), or
2. **`view_all_logs`** — a new, explicit fleet-wide transcript permission,
   implied by `PermissionAdmin` (so the admin API key, `fleet_admin_*` keys, and
   `role=admin` users keep their fleet-wide view) and granted by **no** role
   otherwise.

The decision lives in one place — `taskLogsReadable` / `logReadableTask` in
`internal/sched/handlers/log_authz.go` — and all four transcript routes call it.
The ownership half is the same predicate the workspace gate uses
(`taskCreatedByPrincipal`), so the two artifact classes can no longer drift apart
on who "owns" a task. Concentrating the gate is deliberate: the previous shape was
one check hand-copied into four handlers, which is how they came to agree on a
check that authorized nothing.

`view_all_logs` is mintable without an admin key: `POST /keys` (itself
admin-gated) now accepts an explicit `permissions` array on the legacy
role-based path, validated against the permission vocabulary. It is rejected in
combination with `role` or `type` rather than silently losing to either, so an
operator cannot believe they minted an auditor key that in fact reads nothing.

**Every create path must attribute its creator**, because ownership is now
load-bearing rather than cosmetic. One path did not: the chat `schedule_task`
seam called `Storage.EnqueueTask`, which never set `CreatedBy`, so a task
scheduled from a conversation was owned by nobody — and under this ADR its own
author could not read its transcript. The seam now resolves the approving chat
user's email to a sched user and enqueues through `EnqueueTaskAs`; an email with
no sched account still yields an unattributed task, exactly as before. The
`create_task` tool (a task spawning a task) stays unattributed by design — its
provenance is `CreatedByTaskID`, not a human.

**Workspace files stay stricter.** `taskWorkspaceOwned` remains admin-or-creator
and is *not* widened by `view_all_logs`: a designated transcript auditor is not
thereby granted the file bytes a run produced. Widening workspace access is a
separate decision, and this is not it.

## Enforcement

`internal/sched/handlers/log_authz_test.go` proves the gate against real
Postgres, through the real `AdminOrUserAuthMiddleware`:

- a `fleet_task_*` key that did not create the task, and a `fleet_readonly_*`
  key, are 403'd on **all four** transcript routes (the history and stream
  siblings are asserted explicitly, because the original bug was one weak gate
  copied four times);
- the creating key reads its own transcript, history, and stream;
- the admin key and a key minted with `view_all_logs` read a transcript created
  by nobody; a plain `view_logs` key does not;
- for user principals: the creating user reads its own, another user is 403'd,
  an `admin`-role user reads any;
- an unknown task still degrades to a clean 404, so the dashboard's empty-state
  path is unchanged;
- `POST /keys` grants an explicit permission set and 400s `permissions`+`role`,
  `permissions`+`type`, and an unknown permission string.

`cmd/fleet/task_scheduler_budget_test.go` covers the attribution half: the chat
seam records the approving user (case-insensitively), and an email with no sched
account still enqueues, unattributed.

## Consequences

- **A `readonly` key can no longer read transcripts it did not create** — and a
  readonly key creates nothing. This is the intended break: fleet-wide read
  access is now a decision an operator makes by minting a key with
  `view_all_logs`, not a side effect of a key type. Existing keys are unchanged
  on the wire; what changes is what they may read.
- **Non-admin *users* lose the fleet-wide transcript view** in the Operations
  Center, keeping their own tasks. This mirrors what workspace files have always
  done. Promoting a user to `role=admin` restores it.
- **Chat-scheduled tasks now record their creator.** Besides making them
  readable by their author, this gives them a push audience for the first time
  (`Pool.ownerEmail` resolves through `CreatedBy`), so a chat user who has
  subscribed to task push notifications will start receiving them for tasks they
  scheduled from a conversation. That is the behavior the notification feature
  always intended; it simply never fired for this path.
- **Every transcript route now loads the task row** to authorize — it cannot
  decide ownership otherwise. Streaming a task id with no task row is now a 404
  rather than a stream; in production a task cannot run without its row. The
  stream's terminal-status frame reuses the row the gate loaded, so the request
  makes one `GetTask` call, not two.
- **Residual, documented rather than fixed here:** the task *row* itself
  (`GET /tasks`, `GET /tasks/{id}`) is still fleet-wide under `view_tasks`, and
  it carries `prompt` and `result`. Tightening `/tasks/{id}/output` alone would
  be theater while the row that embeds the same result stays open, so task-row
  scoping is left as its own decision with its own list-filtering design. What
  this ADR closes is the transcript — the artifact that carries the tool traffic,
  not just its summary.

## Alternatives considered

- **Make `taskVisibleToScopes` mean something again.** Rejected: it is
  node-pattern machinery for a routing concept that no longer exists (ADR-0011
  removed the worker-node registry), and every typed key is unscoped by default,
  so the check would still not fire for the exact principal class in the threat
  model.
- **Restrict `view_logs` to admins outright** (no new permission). Rejected: it
  removes the legitimate case — a task key tailing the run it just submitted —
  and would push automations toward an admin key, which is strictly worse.
- **Reuse `taskWorkspaceOwned` unchanged for transcripts** (admin-or-creator,
  no fleet-wide grant). Rejected: it leaves a monitoring/auditing key no option
  but the admin credential. The separate permission is what lets an operator
  grant transcript reads without granting everything else admin implies.
