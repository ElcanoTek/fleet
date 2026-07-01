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
  read/write its shared memory.
- Sharing always targets the creator's **own** team (the server resolves it —
  you cannot share into a team you don't belong to).

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

## Shared project memory

`GET/POST /projects/{id}/memories`, `DELETE /projects/{id}/memories/{memID}` —
any member reads/writes; rows carry the writer's email as provenance and die
with the project. Typed exactly like personal memories (#515 kinds).

## Export / audit

`GET /projects/{id}/export` returns the full project config plus runtime-state
references (shared memories verbatim + member conversation ids) as one JSON
document — auditable without any client content entering fleet core.

## Honest scope (deferred)

- Folder UI is unchanged — the sidebar "view over projects" refit, per-project
  scheduled tasks/triggers/eval-set/skill bindings, and model policy (allowed
  models / max cost / eval-gate-before-model-change) are follow-ons.
- No per-project RBAC beyond the team trust-group (no roles inside a project).
- Project deletion detaches conversations (history belongs to its users) and
  deletes shared memories (they are project state).
