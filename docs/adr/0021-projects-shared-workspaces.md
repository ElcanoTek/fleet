# ADR-0021: Projects bind context; membership rides the team trust-group

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Folders are per-user labels with no config. Issue #509 asks for the org
primitive competitors ship: a shared workspace binding instructions,
connectors, shared memory, and membership, inherited by every conversation in
it. fleet already has the pieces (per-conversation MCP opt-in, typed memories
#515, team RBAC #237); what's missing is the binding object.

## Decision

1. **A `projects` table in the chat store** (migration 028) holding the
   binding: instructions, curated optional-MCP names, default persona/model,
   owner, team.
2. **Membership = the ADR-0013 team trust-group.** No membership table: a
   project's `team_id` grants every matching user access; the owner alone
   mutates. Sharing resolves the creator's OWN team server-side, so nobody
   shares into a foreign team. (A real membership table can be promoted later
   without changing the read-gate shape — same reasoning as ADR-0013.)
3. **Inheritance at creation, injection at turn time.** Creation copies the
   project's defaults + connector selection onto the conversation (the
   existing per-conversation mechanisms — no new governance path); each turn
   injects the instructions as a prompt section and the project's shared
   memories as tagged bullets. Credentials stay host-side; the curated
   connector list is a SELECTION over the same global catalog, never new
   access.
4. **Project memory is a scope, not a copy**: `memories.project_id` set =
   shared row (member-visible, injected in project chats, dies with the
   project); personal queries exclude project rows so the scopes never bleed.

## Alternatives rejected

- **Folders as the entity** — folders are names, not rows; retrofitting config
  onto a string key breaks the moment two users name folders alike.
- **A membership table now** — premature for the 10–20 user box (ADR-0013's
  exact argument); the team trust-group delivers sharing today.
- **Copying project memories into each member's personal store** — divergence
  and deletion anomalies; a scope column keeps one source of truth.
