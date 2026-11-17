# Home chat

Teams that start the day from one chat (a morning patrol's dashboard, say)
wanted Fleet to open on it. A person can now choose one chat as their **Home**:
it is the first row of their chat list, marked with a **Home** badge, and a
tab's first visit lands on it. This note records what shipped, how it works, and what was left out on
purpose. The decision to keep it per person, outside the conversation row, is
[ADR-0113](adr/0113-home-chat.md).

## What shipped

- **One Home per person.** Any chat the person can open can be their Home:
  - one of their own;
  - a **Multiplayer** chat they take part in;
  - a chat a teammate shared with their team (read-only).

  Choosing another replaces it.
- **Choosing it.** **Set as home** / **Remove as home** in a chat's `⋮` menu.
  Rows the person does not own (**Team chats**, and a Home reached through a
  share) get a small `⋮` with only that item. Setting a Home confirms in a
  toast that names the chat it replaced, if any.
- **The Home row.** The Home chat is the first row of the chat list, above
  **Pinned**, with a **Home** badge (map-pin and the word). It is not repeated
  in **Pinned** or **Team chats**. A teammate's chat shows the team glyph and
  the owner's short name, as in **Team chats**. In a project's list an owned
  Home filed there carries the same badge.
- **Landing.** A cold `/chat` boot opens, in order:
  1. a `?c=` link;
  2. a `?team=` link of any outcome (an owner's opens like `?c=`; a teammate's
     viewer or notice is never replaced);
  3. a `?persona=` new-chat link;
  4. Home, on the tab's first visit;
  5. the most recent chat, as before.

  "First visit" is a `sessionStorage` flag (`home-landed`). A new tab or
  window lands on Home; a reload in the same tab does not, and opens the most
  recent chat as before (a reload keeps no record of the chat that was open).
  Signing out clears the flag (`forgetHomeLanding` in `signOut.ts`), so the
  next sign-in in that tab lands on Home. Storage that throws counts as a
  first visit. The flag is only consulted when nothing above Home won, so a
  visit that opened a link does not use up the tab's landing.
- **Opening it the way the person can reach it.**
  - Their own chat loads as a normal chat (its row is an ordinary row). If it
    cannot be loaded at boot
    (deleted or archived since the list was read), the most recent other
    chat opens instead.
  - A Multiplayer chat loads as a normal chat. Opened from its row, it falls
    back to the read-only viewer when the room is closed to them but the team
    share is not (if neither is open, the viewer says so). The boot landing
    does not fall back: it opens the most recent chat instead, so a late
    fallback never covers a project opened meanwhile.
  - A team share opens the viewer directly. On a cold boot the most recent
    chat loads underneath, as a team link already does, so **Back** leaves the
    viewer onto a chat.
  - The landing is a boot restore: it never undoes a project or chat the
    person opened while the first requests were in flight.
- **Keeping the Home row current.** Every active-list refresh reports Home.
  Deleting (one chat or a selection) or archiving one's own Home clears it in
  the rail at once. A warm return from another surface shows the snapshot's
  Home until the background revalidation reads it again.

## How it works

### Storage and API

- **Table.** `chat.user_home_conversation` (migration 0202) holds one row per
  person (`user_email` primary key), so "one Home" is the key rather than a
  rule each writer keeps.
  - Both references cascade. Deleting the account removes the row, so a new
    account later made under the same address does not inherit it.
  - A hard-deleted chat (the retention sweep, a purge) takes the row with it.
    A soft-deleted chat leaves it for the read-time check to hide.
- **Access is decided on read, never stored.** `store.homeAccess` tries, in
  order:
  1. the caller's own chat (not deleted, not archived);
  2. `GetMultiplayerConversation`;
  3. `GetTeamVisibleConversationMeta`.

  These are the same gates each chat's own read path uses, so Home never opens
  anything the rail could not. A refused read reports no Home but keeps the
  row, so a re-share brings Home back.
- **Routes.**
  - `GET /api/me/home` → `{"home": {conversation_id, access, title,
    owner_email} | null}`.
  - `PUT /api/me/home` with `{"conversation_id"}` makes a chat Home. A chat the
    caller cannot open is a 404, the same answer as every other read, so the
    route cannot be used to probe for chats. A blank ID is a 400.
  - `DELETE /api/me/home` clears Home.
  - All three sit behind `rejectViewerWrites`, like `/connector-prefs` and
    `/me/team`: a read-only viewer can read a Home but not choose one. The rail
    offers the menu items only once `/api/me` has confirmed a role other than
    `viewer`, as it does for Reorder.
- **The list carries it.** The active `GET /api/conversations` answer has a
  `home` field, so a cold boot learns where to land from a request it already
  makes, and every list refresh picks up a Home that an unpin, archive or lost
  share changed elsewhere. The field is best-effort: a failed lookup is logged
  and reported as no Home rather than failing the list. The archived list
  (`?archived=true`) has no `home` field.

### Keeping an owned Home

The retention sweep, the unpinned cap and **Delete temporary chats** each
spare pinned chats. So setting your own unfiled chat as Home also pins it, in
the same transaction, rather than teaching each of those paths about Home.

The other direction holds too. `SetPinned(false)`, a bulk unpin (`BulkPatch`)
and `SetArchived` clear the owner's own Home in the same transaction, so an
owned Home is never left unprotected. The rail says so in a toast when the
person unpins their Home. Other people who chose the same chat keep their
Home: the owner's pin was never what kept it.

A chat filed in a project is pinned too. The project exempts it today, but
every path that unfiles a chat (a move, a revoked share, a deleted project)
would otherwise leave it unprotected. Filing clears any other chat's pin but
keeps a Home's (`setConversationProject`). A pin on a filed chat shows nowhere
in the rail.

Lock order is the same everywhere, so none of these can deadlock another:

- `SetHomeConversation` writes the Home row (whose foreign key locks the
  person's `core.users` row) before pinning the chat, the users-then-chat
  order the Multiplayer mutation uses.
- `SetPinned`, `SetArchived` and `BulkPatch` clear the Home row before
  updating the chat, the same Home-then-chat order.
- Filing locks the chat in a statement of its own before its `UPDATE`. Under
  READ COMMITTED the `UPDATE`'s "is this the owner's Home?" subquery reads the
  snapshot its statement started with; locking first means it starts after a
  concurrent `SetHomeConversation` has committed, so it cannot miss the Home
  and clear its pin.

### Client

- `web/src/app/chat/ui/homeChat.ts`:
  - the wire shape (`parseHome`);
  - the calls (`saveHome`, `removeHome`);
  - the boot rule (`bootTarget`, `claimHomeLanding`).
- `chat-experience.tsx` holds `home`, opens it (`openHome`), and keeps `home`
  and `teamChatView` in the warm-return snapshot (`chatSessionStore.ts`). A
  return from Settings therefore keeps a read-only Home on screen.
- The keyboard `j`/`k` walk (`visibleConversationOrder`) puts the person's own
  Home first, where the sidebar renders it.
- `ConversationSidebar.tsx` renders the Home row and its `HomeBadge`.

## Deviations from the plan

- **Unpinning your Home does not ask first.** It unpins, clears Home, and says
  so in a toast. A confirm on every unpin of one chat was more friction than
  the change is worth, since setting Home again is one click.
- **No Home item in the rail's navigation.** An earlier cut added **Home**
  (*Your starting point*) above **Chat**. It cost about 49px of the chat list
  on every screen and duplicated the row, so Home is only the badged first
  row of the list. Reaching Home from Settings or Background tasks is
  **Chat**, then the row.
- **No house icon.** Flag's sprite has no house glyph, so the badge is the
  word **Home** with `map-pin`. A house glyph is to be requested from Flag,
  not drawn here (see `web/AGENTS.md`).

## Deliberately not done

- **No team-wide default Home.** v1 is personal. A team admin setting one Home
  for everyone (with a personal override) is the natural next step for the
  morning-patrol case, and the per-person row does not stand in its way.
- **Not in the terminal chat or ACP clients.** They open whatever chat they are
  told to.
- **A viewer cannot choose a Home**, matching every other per-person setting
  behind `rejectViewerWrites`.
- **Home is not pinned for a teammate.** The owner's pin is theirs. A
  teammate's Home survives as long as the share does; if the owner deletes the
  chat, it goes.

## Tests

- `internal/store/home_conversation_test.go` (Postgres):
  - own chat: pinned with it, replaced by a second choice, clear keeps the pin;
  - refusals: unknown, someone else's private chat, deleted, archived;
  - unpin, bulk unpin and archive clear an owned Home; pinning others keeps it;
  - team share: set, refused to outsiders, survives the owner's unpin, hidden
    by an unshare and back on re-share;
  - Multiplayer: set, then removal from the room drops to `team_view`;
  - cascades: account deletion and hard chat deletion;
  - a filed Home is pinned, keeps its pin when re-filed, and survives being
    unfiled and **Delete temporary chats**; an ordinary chat still loses its
    pin when filed.
- `internal/httpapi/home_test.go`:
  - `/me/home` GET/PUT/DELETE through the routed mux;
  - the `home` field on the list (absent on the archived list);
  - 404 for an outsider and an unknown ID, 400 for a blank ID, 405;
  - viewer read-only.
- `web/src/app/chat/ui/homeChat.test.ts`:
  - `bootTarget` precedence, including that a link never uses up the landing;
  - `parseHome`;
  - `claimHomeLanding` once per tab.
- `web/src/app/chat/ui/ConversationSidebar.home.test.tsx`:
  - the Home row first, with its badge, and not in Pinned;
  - a team Home with its owner and badge, dropped from Team chats;
  - the menu items on own, team and Home rows, and none without handlers.
- `conversationOrganization.test.ts`: the `j`/`k` walk puts an owned Home
  first.
- `web/e2e/mocked/home-chat.spec.ts`:
  - a tab's first visit lands on Home, shown first with its badge;
  - a reload opens the most recent chat; the Home row opens Home again;
  - a `?c=` link outranks Home;
  - **Set as home** sends `PUT /api/me/home` and moves the Home row.
