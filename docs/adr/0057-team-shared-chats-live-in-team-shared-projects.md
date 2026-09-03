# ADR-0057: A team-shared chat lives inside a team-shared project

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** fleet maintainers

## Context

ADR-0013 gave a conversation owner a per-chat opt-in — `conversations.team_visible`
— and one team-scoped list endpoint (`GET /conversations?scope=team`). It has
been in the store since #237 and **no UI ever exposed it**: no toggle set the
flag, no screen read the list, and no route read a single such conversation.
Meanwhile Settings → Team's own copy told users a conversation could be shared
with the team "from its own menu", which was simply not true — the chat kebab's
Share was link-only.

ADR-0021 shipped the other half: a project shares its *definition* (standing
instructions, curated connectors, defaults, shared memory) with a team, and
explicitly **not** its members' chats. That asymmetry is what users kept asking
about: they wanted to see the report a colleague built, or at least the logic
behind it, and were maintaining an off-platform share drive to do it.

So the capability existed and the surface did not. Exposing it raised one design
question that the backend does not answer on its own: **where does a
team-shared chat appear?** `?scope=team` is a flat list of every team-visible
conversation across the whole deployment. A flag with no home produces a chat
that teammates can read but cannot find, and — worse — that its owner has no
obvious place to revoke, because nothing displays its state.

## Decision

**Team sharing is offered only for a chat that is inside a team-shared project,
and the pairing is maintained by the write paths, not by hope.** And because a
project is now where a team's shared work accumulates, it must be able to
outlive its owner: ownership is transferable, and deleting an owner no longer
takes the project with them.

The project home is the one discovery surface: a **Team** section lists the
team-shared chats members contributed *to that project*. That gives every
team-shared chat exactly one place to be found, and gives its owner a place that
displays its state.

Concretely:

1. **The UI narrows what the API allows.** `POST /conversations/{id}/share-with-team`
   is unchanged and still accepts any conversation the caller owns (ADR-0013's
   contract; an existing API client keeps working). The Share dialog offers the
   toggle only inside a team-shared project, and says which situation the reader
   is in otherwise ("move it into a team-shared project" / "this project isn't
   shared with your team — share the project first").
2. **The store maintains the pairing.** Every way of removing a chat's home also
   clears `team_visible`, in the same statement or transaction as the change
   that caused it:
   - moving the chat to no project, or to a personal one (`SetConversationProject`),
   - turning off a project's team sharing (`UpdateProject`),
   - deleting the project (`DeleteProject`, which already detached chats),
   - **leaving the team** (`SetOwnTeam("")`), which unshares the leaver's chats
     in that team's projects — otherwise a chat stays readable by a group its
     owner is no longer in, with no surface left on their side to revoke it.
3. **Reading one shared chat is a new, narrow route.**
   `GET /conversations/{id}/team-view` is the only conversation route a
   non-owner may reach. Two gates, neither sufficient alone: a shared
   non-empty `users.team_id` **and** the owner's per-chat opt-in. It returns
   the **transcript only** — user/assistant text, the same filter the public
   share snapshot applies — never tool calls, tool results, or reasoning, whose
   content can carry command output and API responses that were never part of
   what the owner shared. Attachments and generated files stay behind the
   owner-scoped workspace route: a shared conversation *about* a report must not
   hand out the report. Every refusal is a 404, indistinguishable from "no such
   chat", so team membership is not probeable.
4. **Building on someone's work is Branch, not co-authoring.**
   `BranchConversation` accepts a parent the caller can *read* — their own, or a
   team-shared one — and files the fork into the parent's project when the
   brancher is a member of it. The fork is a copy owned by the brancher from the
   first byte: private until they share it, unaffected when the original is
   unshared or deleted, and requiring no write access to the original.
   Conversations keep exactly one owner.
5. **A project can change hands.** `POST /projects/{id}/transfer`, authorized
   for the owner **or an admin**, moves `projects.owner_email` and nothing
   else. The admin arm is the reason the route exists: every project mutation
   is owner-scoped, so a departed owner left a frozen definition, and
   `DeleteUser` — the routine "X left the company" action — deleted their
   projects along with every team learning in them, for people still using
   them. `DeleteUser` now fails closed on an account owning team-shared
   projects and names them; personal projects still go with the account.
6. **Two audiences, two badges.** Share-by-link and share-with-team are
   independent (a chat can carry one, both, or neither) and are drawn with
   different glyphs, each always labeled with its audience. One unlabeled chain
   link previously stood for the only scope that existed, which inside a
   team-shared project read as "my team can see this" — a reasonable and wrong
   inference.

## Enforcement

- `internal/store/team_sharing_test.go` — the read gates (teammate yes, other
  team no, teamless no, unshared no; tool entries filtered out), all five ways
  the pairing is maintained, the project-scoped Team listing, branching from a
  shared chat, the leave/delete impact counts, ownership transfer (including
  the cross-team refusal), and that deleting an owner of a team-shared project
  is refused before anything is destroyed.
- `internal/httpapi/team_sharing_http_test.go` — `team-view` over HTTP including
  the 404-for-everything-else rule, a teammate's branch landing in the project,
  the Team section and delete-impact endpoints, the team-learnings permission
  gate, transfer by the owner and by an admin (and the 404 for anyone else),
  and the admin delete's `409` naming the projects to transfer.
- `internal/httpapi/conversation_routes_test.go` — `team-view` is in the
  dispatch table and stays in sync with it.
- `web/src/app/chat/ui/ShareDialog.test.tsx` — the UI narrowing and its copy,
  and that opening the dialog never mints a public link.
- `web/src/app/chat/ui/ProjectHome.team.test.tsx` — the Team section, its empty
  state, and its absence in a personal project.

## Consequences

- **A second cross-user conversation read path exists**, joining
  `ListTeamConversations`. It carries the same two gates, and is the only one
  that returns a transcript. Both remain opt-in per conversation; a shared
  `team_id` still exposes nothing on its own.
- **`team_visible` is now cleared by writes that used to leave it alone.** An
  API client that set the flag on a chat outside a project keeps it (nothing
  clears a flag on a chat with no project to lose), but one that later files
  such a chat into a personal project will find it unshared. That is the
  invariant working, and it is stated in the docs rather than silent.
- **`conversations.team_visible` is now serialized** on the owner's own
  listings so the rail can badge it. It was already the owner's own state; no
  other user's flag is exposed.
- **The narrowing is UI-only, deliberately.** The backend stays as ADR-0013
  wrote it, so this is a product decision we can revisit (e.g. a "shared with
  team" section outside projects) without a schema or authorization change.
- **Leaving a team is now destructive in a way it was not.** It unshares the
  leaver's chats. Settings → Team therefore confirms first and quotes the
  counts, rather than acting and reporting afterwards.
- **Deleting a user can now fail.** An account owning team-shared projects is
  refused until they are transferred. That is a new way for a previously
  always-succeeding admin action to stop, and it is the point: the old
  behavior's success was data loss for everyone else on the team.

## Alternatives considered

- **Expose `?scope=team` as a flat "Shared with me" section in the rail.**
  Rejected for this pass: it is a second organizing axis competing with
  projects, and it answers "what has my team shared?" without answering "where
  does this belong?" — the campaign-shaped question users actually asked. The
  backend for it is untouched, so it stays available.
- **Enforce the narrowing server-side (refuse `share-with-team` outside a
  team-shared project).** Rejected: it would break ADR-0013's documented
  contract for an existing API, to buy a guarantee the pairing rules already
  give — any chat sharing that gets past the UI still loses the flag the moment
  it has no home.
- **Give teammates write access to a shared chat.** Rejected. A conversation is
  a turn-by-turn state machine with one workspace, one sandbox, and one cost
  ledger; two writers is a different feature. Branch gives the same outcome
  (build on the work) with none of that.
- **Share the chat's workspace files with the team too.** Deferred. The
  transcript is what "see how they did it" needs, and a conversation about a
  confidential report must not become a download link for it. A future pass
  could add per-file sharing with its own opt-in.
