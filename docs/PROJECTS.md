# Projects / Spaces: shared team workspaces

A **project** is the binding object folders never were (#509, ADR-0021):
standing instructions, a curated connector selection, default persona/model, a
**shared memory scope**, and membership — and every conversation started in it
inherits that context automatically.

## Membership

Reuses the #237 team RBAC trust-group — no new membership table:

- A project with a `team_id` is visible/usable by every user whose
  `users.team_id` matches, plus the owner.
- An empty `team_id` = personal project.
- **Only the owner edits or deletes** the definition; members chat in it and
  read/write its shared memory. Ownership can be **transferred** —
  `POST /projects/{id}/transfer {"to_email": …}`, by the owner or an admin — so
  "the owner left" is recoverable; see [`TEAM-SHARING.md`](TEAM-SHARING.md).
  Deleting an account that still owns a team-shared project is refused (`409`)
  until it is transferred.
- Sharing always targets the creator's **own** team (the server resolves it —
  you cannot share into a team you don't belong to).

### Getting a team (#1157, ADR-0047)

Sharing needs a team, and until #1157 `users.team_id` was writable only by an
admin — a path that was itself broken for an `ADMIN_EMAILS` bootstrap admin, so
a fresh box had no way to create a team and every "Share with my team" failed.
Team membership is now split by what the write grants:

- **Create / leave: self-serve.** `PUT /me/team {"team_id": "platform"}` (`""`
  leaves), surfaced as **Settings → Team**. The Projects modal briefly offered
  the same create inline; it no longer does (ADR-0057) — its copy pointed
  teammates at a "join the same name" path the server refuses, and a
  once-per-account act does not belong in a per-project dialog. A teamless
  caller is told where to go, branched on whether they are an admin.
  `GET /me/team` additionally reports what LEAVING would cost, so the confirm
  can state it before acting.
- **Join an existing team: granted.** A name another user — or a team-shared
  project — already holds is refused with `409` ("ask an admin to add you to
  it"); an admin adds you from **Settings → Admin → Users**. A shared `team_id`
  is what exposes team-shared projects and team-visible conversations, so it is
  never claimable by typing a name.

`GET /me` (and `GET /me/team`) reports `{email, role, team_id, admin}` — the UI
reads it to name the team on the share control and to phrase the "ask an admin"
path. Renaming a team afterwards is `POST /admin/teams/rename`, which relabels
members and team-shared projects in one transaction.

## Inheritance (at conversation creation)

`POST /conversations {project_id}` validates membership and seeds:

- **persona / model** from the project's defaults (explicit request values win;
  lockdown model rules still apply),
- **optional-MCP opt-in** from the project's curated `mcp_servers` (names from
  the same global catalog; credentials stay host-side exactly as for any
  conversation-level opt-in),
- the conversation's `project_id` binding (set once at creation).

Every turn in a project conversation then injects:

- a `## Project Instructions` system-prompt section (the standing
  instructions), and
- the project's **shared memories** as `[project]`-tagged bullets alongside
  personal memory (project-scoped rows are excluded from everyone's personal
  memory lists — the scopes never mix; #515 coordination).

## Shared project memory — "team learnings" to users

`GET/POST /projects/{id}/memories`, `PATCH/DELETE /projects/{id}/memories/{memID}`
— any member reads and writes; rows carry the writer's email as provenance and
die with the project. Typed exactly like personal memories (#515 kinds).

**Changing an existing entry is narrower than writing one:** its author, or the
project owner. `PATCH` covers pin / edit / retire, and **retire is the default
remove** — the entry stops being injected, the record of what was learned and by
whom survives. `POST` with `{"from_memory_id": …}` MOVES one of the caller's own
personal memories into the project (the promotion path), rather than copying it.

Users never see the words "shared memory": the label everywhere in the UI is
**Team learnings**, listed with author and date on the project home and in the
composer's memories modal. See [`TEAM-SHARING.md`](TEAM-SHARING.md).

## Team-shared chats (ADR-0057)

A project shares its definition, never a member's chats — with one exception the
owner opts into per chat. `conversations.team_visible` (ADR-0013) is surfaced by
the Share dialog **only for a chat inside a team-shared project**, and the
project home grows a **Team** section listing what members shared there:

- `GET /projects/{id}/team-conversations` — the section's list (other members'
  shared chats in this project; the caller's own already show under their chats).
- `GET /conversations/{id}/team-view` — the read-only transcript, gated on the
  caller's `team_id` matching the audience the owner **named** when they shared
  (`conversations.team_shared_with`, migration 054) AND the owner's opt-in
  still being on. Transcript only: no tool calls, no reasoning, no workspace
  files.
- `POST /conversations/{id}/branch` accepts a parent the caller can read, so a
  teammate builds on the work by forking it into a chat they own. A fork of
  someone else's chat copies only what `team-view` showed, and keeps the
  parent's lockdown.

`POST /conversations/{id}/share-with-team` refuses (`409`) unless the caller is
in a team and the chat is in a project shared with that team, and reports the
state it stored. Every write that takes a chat's home away clears the flag and
the audience with it: moving it out (or into another team's project),
un-sharing the project (or re-sharing it with a different team), deleting the
project, leaving the team, and being moved between teams by an admin. Un-sharing
is never refused. Details and rationale:
[`TEAM-SHARING.md`](TEAM-SHARING.md) + ADR-0057.

## Export / audit

`GET /projects/{id}/export` (owner or admin — it carries every member's
conversation ids) returns the full project config plus runtime-state
references (shared memories verbatim + member conversation ids) as one JSON
document — auditable without any client content entering fleet core.

## Retention

Filing a chat into a project is a **keep** state: project conversations are
exempt from the TTL sweep, the unpinned-cap eviction, auto-archive, **and** the
bulk "Delete all unpinned" / label-filtered bulk delete. (The bulk paths were
the gap — they deleted filed chats, which made the UI's "chats in a project
don't expire" false; fixed with `project_id IS NULL`, covered by
`internal/store/team_sharing_test.go:TestBulkDeleteSkipsProjectChats`.)
Deleting a project detaches its chats, which drops them back into Temporary —
so the delete confirm says so, and quotes the counts.

## Honest scope (deferred)

- Folder UI is unchanged — per-project scheduled tasks/triggers/eval-set/skill
  bindings, and model policy (allowed models / max cost /
  eval-gate-before-model-change) are follow-ons.
- No per-project RBAC beyond the team trust-group (no roles inside a project),
  and no invitations: adding someone to an existing team is an admin action
  (ADR-0047), not a request the invitee can send or accept.
- Project deletion detaches conversations (history belongs to its users) and
  deletes shared memories (they are project state).
- No campaign lifecycle (archive/complete), and no automatic proposal of team
  learnings from repeated corrections — the #516 feedback loop is still
  task-scoped. See [`TEAM-SHARING.md`](TEAM-SHARING.md) "Honest scope".
