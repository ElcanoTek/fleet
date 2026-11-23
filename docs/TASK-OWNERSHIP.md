# Task ownership, copy lineage and the create-time connector check

A scheduled task's **owner** is its `created_by` user. The owner decides three
things at run time: whose hosted (remote) connections the run can mount, who
receives the task in the daily owner digest ([OWNER-DIGEST.md](OWNER-DIGEST.md)),
and who may read and edit the task as its creator (ADR-0043). A task's
**lineage** (`lineage_id`, [ADR-0069](adr/0069-per-job-scheduled-workspaces.md))
names the job it belongs to: its per-job workspace directory and its owner-digest
entry.

This note records how copies get an owner and a lineage, how an admin changes a
task's owner, the check that refuses a task whose connectors cannot resolve,
and which tasks a caller may read and act on.

## What went wrong (2026-10-26)

An operator used the bootstrap admin API key to `POST /tasks/{id}/clone` and
`POST /tasks/{id}/rerun` a user-owned recurring Pages-refresh task, overriding
only the prompt, title and tags so that each copy refreshed a different
dashboard.

1. Every copy was minted with `created_by = NULL`. The admin key has no user
   behind it, and the copy belonged to the caller. The source's `mcp_selection`
   named `browserbase`, which resolves only through the owner's hosted
   connections, so every run of every copy dead-lettered at once with
   `mcp selection references server(s) [browserbase] which are neither in the
   active bundle catalog nor among the task owner's hosted connections`.
2. Every copy kept the source's `lineage_id`. Four dashboards' chains shared one
   per-job workspace directory and one owner-digest entry.
3. Nothing refused the selection when the copies were made. It failed only when
   they ran.
4. There was no way to give the ownerless copies an owner: `TaskCreate` has no
   `created_by`, and there was no reassignment endpoint.

## What shipped

### 1. A copy made by a key keeps the source's owner

`POST /tasks/{id}/rerun` and `POST /tasks/{id}/clone`
(`handlers.rerunCreator`):

| Caller | `created_by` of the copy | `created_by_key_id` of the copy |
|---|---|---|
| A user (session cookie) | the caller (unchanged since #270) | none |
| A scoped API key | **the source's owner** | the calling key |
| The bootstrap admin key | **the source's owner** | **the source's key** |

A user's copy is still theirs, including an admin-role user copying someone
else's task. Only a principal with no user behind it changes. The scoped key
stays the authorizing key, because spend is attributed to it and ADR-0043 lets
the key that made a task read it. The admin key has no key id of its own, so the
copy keeps the source's key.

The budget gate's user scope now applies to a key-made copy as well. A copy owned
by a user is checked against that user's budgets, as a create by that user would
be. Before, an admin-key copy was checked against no budget.

There is no override that makes a copy unowned. An ownerless copy is exactly
the failure above.

**Who may copy a task.** Because a key's copy runs as the source's owner (that
owner's hosted connections, budgets, lineage and per-job workspace), only a
caller that may act on the source can copy it: the source's creator (the user,
or the API key in `created_by_key_id`) or an admin (the admin API key, a key
carrying `admin`, an admin-role session). A key that can read the task only
through `view_all_logs` gets 403 `Only the task creator or an admin can re-run
or clone this task`. The table above therefore applies to a scoped key copying
a task it created, and to an admin. `rerunCreator` applies the same rule where
it picks the owner: a key that may not act on the source never receives its
owner. See [Reading is not acting](#5-reading-is-not-acting) for every route.

### 2. `new_lineage`: a copy that is a different job

`overrides.new_lineage: true` on rerun or clone gives the copy a fresh
`lineage_id` (its own id). The copy then has its own workspace directory and its
own owner-digest entry. `source_task_id` still records where it came from. (The
dead-letter breaker was never keyed on lineage: it follows
`previous_occurrence_id`, so every copy already has its own chain there.)

The default is unchanged: a copy joins the source's lineage. **Resubmit with
changes** relies on that, because the rerun works in the directory the failed run
left behind. Use `new_lineage` when the copy points at another dashboard, another
client or another report. The web UI does not expose it.

This does not conflict with ADR-0069's "not settable by clients". A client still
cannot choose a lineage. It can only start a new one, as any fresh create does.

### 3. The create-time connector check

A task whose effective `mcp_selection` names a server that is neither in the
active bundle inventory nor among the **owner's** hosted connections is refused
with HTTP 400. The message is the one its run would have dead-lettered with.

The check is not a second copy of the run's logic.
`scheduledrun.Runner.CheckTaskMCPSelection` reuses the run's own partition
(`splitTaskMCPSelection`), its guard order (`buildTaskRemoteOverlay`), its
membership rule (`pinsNotKnown`, shared with `unresolvedPins`) and its error text
(`unknownMCPSelectionError`, and `agentcore.UnknownMCPServerError` for the
binder's refusal on a deployment without hosted connections). cmd/fleet hands
that method to the handlers (`Handlers.SetMCPSelectionCheck`).
`TestCheckTaskMCPSelection_MatchesTheRun` pins that the create-time error and
the run's error are identical.

There is one deliberate difference. A run builds the owner's overlay, which mints
tokens and dials every server. A create only **lists** the owner's connections,
through the new `scheduledrun.Options.HostedConnections` (wired to
`remotemcp.Service.ConnectedServersForUser`). A connection in the `connected`
state passes. If that connection's login has lapsed, the run skips it with a
notice, exactly as before.

Where it runs, always against the owner the task will have:

| Path | Owner checked |
|---|---|
| `POST /tasks`, A2A `SendMessage` (the shared governed pipeline) | the creator; for the admin key, nobody |
| `POST /tasks/{id}/rerun`, `/clone` | the copy's owner (section 1) |
| `POST /tasks/batch` | the creator, per task (a failure of that task, like the priority ceiling) |
| `POST /tasks/import`, create | the importer |
| `POST /tasks/import`, `conflict=replace` | the existing row's owner, which the replace keeps |
| `PUT /tasks/{id}`, **only when the edit changes `mcp_selection`** | the task's owner |
| `PUT /tasks/{id}/owner` | the **new** owner, for **every row it rewrites** (section 4) |

An edit that leaves the selection alone is not re-checked, so a prompt fix on a
task whose connection lapsed since it was created is not refused.

An admin-key `POST /tasks` creates a task with no owner. Such a task can no
longer name a hosted connection. Its runs could never have resolved one.

The check returns only a definite refusal. It lets the create through, as the
run would proceed, in these cases:

- The owner's email lookup fails.
- The credential allowlist denies every server, so the selection is moot.
- Listing the connections fails.
- No lister is wired.

One known false refusal: if the chat store's connector-preferences read fails,
`ConnectedServersForUser` reports no connections rather than an error, so a
create during that outage is refused. It names the connection, and a retry
succeeds once the store answers.

The check covers server **names**. It does not check the **account seat** a
selection pins on a bundle server. At dispatch the binder (`resolveMCPVariant`)
refuses three kinds of seat:

- an account on an HTTP server;
- a named account whose `<VAR>_<ACCOUNT>` credentials are not set;
- a seat that would inherit the default seat's routing identity.

Deciding that needs the server's credential environment, which by invariant
lives only in the credential-owning broker child. The parent is scrubbed after
boot. A create-time seat check therefore needs a broker round trip, or a second
copy of the binder's rules over a names-only seat list. Both are follow-ups, not
part of this change. Such a task is still refused at dispatch, as before.

Not checked, documented here rather than guessed:

| Path | Why it is not checked |
|---|---|
| The chat `schedule_task` tool and the in-run `create_task` tool | They write through `storage.EnqueueTask*`, below the handlers. |
| `fleet sched task import`, `batch-create`, `set-owner` | They write the sched database directly from a process that has no bundle inventory. |
| Recurrence spawns and trigger runs | They copy a definition that was already checked. |

If the bundle drops a server, or the owner disconnects one, after the task was
created, the run still refuses the selection as before.

### 4. An admin sets a task's owner

```
PUT /tasks/{task_id}/owner
{"username": "<username or email>"}
→ 200 {"task": <Task>, "updated_task_ids": [<task_id>, …the chain rows it reached, in chain order]}
```

The endpoint is admin only. The check is `PermissionAdmin` in the handler, so the
admin API key, a scoped key carrying admin, or an admin-role session all qualify.
Other responses:

| Status | Cause |
|---|---|
| 400 | Empty username, or the selection of a row it would rewrite does not resolve for the new owner (the message names that row; nothing is written) |
| 403 | The caller is not an admin |
| 404 | `Task not found`, or `User not found` (unknown or disabled user) |

The username is matched as given, then lowercased. Each change is logged with
the task, the new owner, the actor and the number of rows changed.

CLI twin: `fleet sched task set-owner <task_id> <username>`. Exit codes are
2 for an unknown user or task and 5 for a storage failure. The CLI writes the
database directly and skips the connector check (see above).

`Storage.SetTaskOwner` sets `created_by` on these rows:

- the task itself;
- every **unfinished** later occurrence of its recurrence chain;
- the chain's **last** occurrence, even when it has finished.

It finds them by following `previous_occurrence_id` forward. It does not use
`lineage_id`, because copies share a lineage with their source, and reassigning
one copied chain must not reassign another.

- **The last occurrence** is included because a chain continues only from it:
  its pending spawn, a reconciliation of a lost spawn, or a replay of a parked
  dead-letter. The 2026-10-26 copies parked after two dead-letters, so their last
  row must carry the new owner, or a replay would resume under the old one.
- **Finished occurrences in between** keep their owner. They are history.
- **Future occurrences** inherit the new owner, because a recurrence spawn copies
  `created_by` from the completing row.
- **Recorded spend** follows the rows that change owner: the same transaction
  sets `created_by` on their `task_iterations`, which carry their own copy of
  the attribution so spend keeps counting after a task is deleted
  ([USAGE-ANALYTICS.md](USAGE-ANALYTICS.md), migration 0210). The usage report
  and the user budgets then count it for the new owner. `--lineage` (CLI only)
  does the same for the unowned historical occurrences it attributes.
- **The connector check** runs on every row in the set, each as it would read
  afterwards (owner = the new owner), while the rows are locked. A next
  occurrence edited to a different selection is checked on its own selection.
  One refusal writes nothing.
- **Concurrent spawns**: every row in the set is locked (`FOR UPDATE`), and the
  set is then recomputed until it is stable. A spawn claims its credit with an
  UPDATE on the completing row, so an uncommitted spawn waits and then copies the
  new owner. A spawn that committed first shows up in the recomputed set as the
  new last row.
- **Cost**: `previous_occurrence_id` has no index, so each step of the walk scans
  `tasks`. A walk from the chain's live occurrence, the usual target, is one or
  two steps. No migration was added for this.

`created_by_key_id` is not touched. It stays immutable provenance (#1270).

### 5. Reading is not acting

A route's permission (`x-fleet-permission` in `docs/openapi.yaml`) admits a
caller to the route. A per-task scope then decides which tasks it reaches:

| Scope | Reaches | Check |
|---|---|---|
| read | the caller's own tasks; every task with `view_all_logs` (admin implies it) | `taskVisibleToPrincipal`, `taskLogsReadable` |
| files | the task's creator, or the bootstrap admin key | `taskWorkspaceOwned` |
| act | the task's creator (user or API key), or any principal holding `admin` | `taskActableByPrincipal` |
| stop | the task's creator; or `cancel_task` on a task the caller can read | `taskStoppableByPrincipal` |
| admin | `admin` only | `PermissionAdmin` in the handler |

`view_all_logs` grants reading: every task's row, output, error analysis and
run-log transcripts. It does not grant workspace bytes or the artifact
manifest, and it does not grant acting. A task runs as its owner, so changing
it, starting a run from it or feeding its run is acting for that owner. The
one non-read it widens is cancelling, and only together with `cancel_task`:
a key holding both can cancel any task it can read, but only the task's
creator or an admin can permanently delete it. A cancel ends the task and
runs nothing as its owner; a permanent delete removes the owner's record and
transcripts for good, so it is in the act scope (`taskDeletableByPrincipal`).

| Route or A2A method | Scope |
|---|---|
| `GET /tasks/{id}`, `/output`, `/error-analysis`, `/stream`; `GET /logs/{id}`, `/history`, `/history/{entry_id}`; `GET /tasks`, `/tasks/upcoming`, `/tasks/export`, `/tasks/tags`, `/tasks/recipients` | read |
| `GET /tasks/{id}/artifacts`, `/workspace`, `/workspace/*` | files |
| `PUT /tasks/{id}`, `POST /tasks/{id}/tags`, `/rerun`, `/clone` | act (plus `create_task`) |
| `DELETE /tasks/{id}/permanent` | act |
| `POST /tasks/{id}/wake` | act (plus `cancel_task`), or the user who created the task |
| `DELETE /tasks/{id}` | stop |
| `PUT /tasks/{id}/owner`; `POST /tasks/import?conflict=replace` | admin |
| A2A `GetTask`, `ListTasks`, `SubscribeToTask` | read |
| A2A `SendMessage` answering a task's question; the four push-config methods (a config carries its creator's token and credentials) | act |
| A2A `CancelTask` | the task's creator, or `cancel_task` on a task the caller can read |

`cmd/fleet/task_route_scope_test.go` walks the real router: every route under
`/tasks` and `/logs`, and every route with a path parameter naming a task,
must have a scope in its table (read, list, files, act, stop, create or
admin). A task in such a path must be spelled `{task_id}`, and every route
with a per-task scope must name one, so none escapes the probe. The read
scopes are GET only (an admin-only GET needs an entry in its allowlist,
checked against the router), and stop is DELETE only. Each route naming a
`{task_id}` is driven with a key holding every permission except `admin`
(against another principal's task and against its own), the bootstrap admin
key, a typed admin key and an admin-role session. `TestEveryA2AMethodHasAScope` (`internal/sched/handlers`)
reads the A2A dispatcher's method switch and requires the same for every
method, and `TestA2AViewAllLogsReachesOnlyReadAndStopMethods` drives them.

**Files a task names.** A run copies each upload in `files` into its
workspace, where the task's creator can download it, so naming an upload hands
out its bytes. `POST /upload` is the only way to stage one; it records who
staged it (`temp_uploads/.uploaders/<name>`, next to the checksum sidecar,
swept with the upload). A principal holding `admin` may name any upload. Any
other caller may name an upload it staged itself, one on a task it created,
or one already on the task it acts on (the task it edits, or the source of a
re-run or clone). Any other name is refused with `file not found: <name>`, the
answer a missing upload gets. The check (`checkCallerFiles`) is one of the
create gates: `admitCreate` and `admitEdit` run it, so it applies to
`POST /tasks`, `/tasks/batch`, `/tasks/import`, `PUT /tasks/{id}` and the
`files` override of re-run and clone, and `POST /tasks/estimate` runs it on
the forecast's body. A re-run or clone checks it against the caller, not the
copy's owner. In a batch
or an import a refused name fails that record (an import dry run reports it
too); both check every record's uploads before the budget, the key's request
counter and any write, so if the lookup itself fails the whole request fails
with a server error and nothing is charged or written.
`POST /upload` stores a run of dots in a file's name as one dot, so every
stored name is one a task can name. A2A, the chat `schedule_task` tool and
the in-run `create_task` tool take no `files`. A trigger run and a recurrence
spawn copy their template's or chain's `files`, which were checked when that
definition was written.
`conflict=replace` on import is admin only. An upload staged before uploads
were recorded can be named only through a task its caller created.

The act scope follows `created_by_key_id` as well as `created_by`. A key that
created a task keeps acting on it after an admin gives the task an owner with
`PUT /tasks/{id}/owner`: its edits and its copies run as that owner, and may
name that owner's hosted connections in `mcp_selection`
(`TestOwnerChangeCarriesTheCreatingKeysActs`).

An admin reaches a job's workspace bytes through a copy. A re-run or clone by
a typed admin key or an admin-role session joins the source's lineage by
default, and that admin is the copy's creator, so it reads the copy's
workspace: the source's per-job directory.

## Recovering the 2026-10-26 copies

For each ownerless copy, give it its owner. Its last occurrence (the parked
dead-letter, or the scheduled next run) is reassigned with it, and later
occurrences inherit the owner. Then replay the parked occurrence to resume the
schedule:

```
fleet sched task set-owner <copy_id> <owner email>
```

The copies still share the source's lineage. A lineage cannot be changed on an
existing row. To separate them, clone each copy with `new_lineage: true` and
then stop the old chain.

## Copies a key made of a task it did not create

A re-run or clone made by an API key keeps the source's owner and records the
key in `created_by_key_id`, so the key is the copy's creator: it acts on the
copy (edits it, re-runs it as that owner, in that owner's lineage) and names
the uploads on it, as it does for any task it created. A copy whose key is not
the source's key, on a task that has an owner, is such a copy. To list them,
run this against the fleet database:

<!-- key-copies-query: TestKeyCopiesQueryListsCopiesOfOtherPrincipalsTasks runs this block -->
```sql
SELECT dup.id                AS copy_id,
       dup.created_by        AS owner,
       dup.created_by_key_id AS copy_key,
       src.created_by_key_id AS source_key,
       dup.source_task_id,
       src.id IS NULL        AS source_deleted,
       dup.status,
       dup.created_at
FROM sched.tasks AS dup
LEFT JOIN sched.tasks AS src ON src.id::text = dup.source_task_id
WHERE dup.source_task_id IS NOT NULL
  AND dup.created_by IS NOT NULL
  AND dup.created_by_key_id IS NOT NULL
  AND (src.id IS NULL OR dup.created_by_key_id IS DISTINCT FROM src.created_by_key_id)
ORDER BY dup.created_at;
```

Rows to expect that need no action:

- copies made by a typed admin key or a key carrying `admin`, which may copy
  any task (check the key with `GET /keys/{key_id}`);
- copies whose source was permanently deleted (`source_deleted`): the source's
  key is unknown, so a key's copy of its own task is listed too.

A session user's copy is not listed (it has no key), nor is a copy made by the
key that created the source, including after an admin gave either an owner.

For a copy the key should not act on: permanently delete it
(`DELETE /tasks/{id}/permanent` as an admin). To keep the job, first clone it
from its owner's session, which makes a copy owned by that user with no key on
it, then delete the old one. A clone with the bootstrap admin key keeps the
source's key, and `PUT /tasks/{id}/owner` does not change `created_by_key_id`,
so neither of those takes the copy away from the key.
