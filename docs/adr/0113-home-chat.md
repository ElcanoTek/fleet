# ADR-0113: A person's Home chat is a per-person row, resolved on read

- **Status:** Accepted
- **Date:** 2026-11-17
- **Deciders:** fleet maintainers

## Context

Teams that start the day from one chat (a morning patrol that posts a
dashboard) asked for Fleet to open on it: one chat per person, at the top of
their chat list, landed on when they open Fleet. The chat they want is often not
theirs. It is a teammate's patrol, shared with the team or open as a
Multiplayer room.

The obvious home for such a flag is next to `pinned` on the conversation row.
It does not fit. A conversation has exactly one owner and every column on it is
the owner's: `SetPinned` is owner-scoped, and a teammate pinning a team chat is
a 404. A flag on the row cannot say "Alice's Home" and "Bob's Home" about the
same chat. A team share is also revocable from many paths (unshare, archive,
leaving the team, re-pointing the project, ADR-0057), and any stored
"how Alice reaches this chat" would have to be kept true on each of them.

## Decision

1. **A per-person row.** `chat.user_home_conversation (user_email PRIMARY KEY,
   conversation_id, updated_at)`. The primary key makes "one Home" structural.
   Both references cascade: an account's deletion takes its row, so an
   address reused by a new account inherits nothing; a hard-deleted chat takes
   every row naming it.
2. **Access is resolved on every read, never stored.** The store tries the
   caller's own chat, then `GetMultiplayerConversation`, then
   `GetTeamVisibleConversationMeta`. These are the gates each chat's own read
   path uses, so Home can never open a chat the rail could not. A refusal
   reports no Home and keeps the row. None of the revocation paths needs to
   know about Home, and a re-share brings it back.
3. **An owned Home is pinned, and unpinning clears it.** Setting one's own
   chat as Home pins it in the same transaction; `SetPinned(false)`,
   `BulkPatch` unpin and `SetArchived` clear the owner's Home in theirs. The
   retention sweep, the unpinned cap and "Delete temporary chats" already
   spare pinned chats, so none of them has to learn about Home either. Filing
   clears a pin, except a Home's, so a Home that later leaves its project
   (by any path) is still pinned.
4. **Setting Home answers like a read.** `PUT /me/home` on a chat the caller
   cannot open is a 404, the same as every other read, so it cannot probe for
   chats. The route is behind `rejectViewerWrites` like every per-person
   setting.

## Consequences

- A teammate's Home is only as durable as the share. If the owner unshares,
  Home is hidden; if they delete the chat, it is gone.
- Every read of Home costs up to three indexed lookups. It runs once per
  active-list request, which the chat makes on boot and on refresh. The lookup
  is best-effort there: a failure costs the landing spot, not the list.
- A team-wide default Home (set by an admin, overridable per person) can be
  added beside this row without changing it.
