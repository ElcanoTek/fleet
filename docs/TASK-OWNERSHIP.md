# Task ownership, copy lineage and the create-time connector check

A scheduled task's **owner** is its `created_by` user. The owner decides three
things at run time: whose hosted (remote) connections the run can mount, who
receives the task in the daily owner digest ([OWNER-DIGEST.md](OWNER-DIGEST.md)),
and who may read and edit the task as its creator (ADR-0043). A task's
**lineage** (`lineage_id`, [ADR-0069](adr/0069-per-job-scheduled-workspaces.md))
names the job it belongs to: its per-job workspace directory and its owner-digest
entry.

This note records how copies get an owner and a lineage, how an admin changes a
task's owner, and the check that refuses a task whose connectors cannot resolve.

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
| A user (bearer token, session cookie) | the caller (unchanged since #270) | none |
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
