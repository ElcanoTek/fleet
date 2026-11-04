# Chat order in the rail

People with many chats asked to put them in their own order rather than
most-recent-first. This note records what shipped, how it works, and what was
left out on purpose.

## What shipped

- **Your own order for pinned chats.** With two or more pinned chats, the
  **Pinned** heading shows **Reorder**.
- **Your own order for a project's chats.** A project's options menu offers
  **Reorder chats** when the project holds two or more of your chats. It
  expands the project and puts its chats in reorder mode.
- **A sort for Temporary chats.** The **Temporary** heading has a sort control:
  **Recent activity** (the default and the old behaviour), **Newest first** (by
  when the chat was started) or **Title A–Z**. The choice is remembered on this
  device (browser storage, `fleet.rail.recentSort`), not saved to the account.
  Temporary chats expire with inactivity, so they get a sort rather than a
  hand-set order: a hand-placed row would keep being displaced by new activity,
  and would vanish when the chat expired. To give a chat a place of its own,
  pin it or file it in a project.

### Reorder mode (`ChatReorderList.tsx`)

- The section swaps its ordinary rows for a list. Each item can be dragged, or
  moved one place with its up/down buttons. The buttons are how the list works
  from the keyboard and on touch screens, where HTML drag and drop does not
  fire.
- Each move is announced through a polite live region.
- **Done** or `Esc` leaves the mode. `Esc` is taken in the capture phase and
  stopped there, so on a narrow screen it does not also collapse the rail
  overlay or close the sidebar.
- Focus behaviour:
  - Entering the mode moves the focus to the list's first control.
  - After an arrow move, focus stays on that chat's arrow, or moves to its
    other arrow when the first one is now disabled at an end.
  - Leaving the mode returns the focus to what opened it: **Reorder**, or the
    project's options button.
- The drag uses its own MIME type, so a reorder drag is never taken for the
  existing drag of a chat into a project.
- Only one scope is in reorder mode at a time.
- **Who is offered it.** The mode is offered for 2 to 500 chats in a scope;
  500 is the server's cap. It is not offered to a read-only `viewer` account,
  whose writes are refused (the role comes from `GET /api/me`).

### Storage and API

- **Saved per user, on the server, by scope.** A scope is `pinned` or
  `project:<project id>`. `GET /api/conversations/order` returns every saved
  order (`{"orders": {"pinned": [...], "project:p1": [...]}}`).
  `PUT /api/conversations/order` with `{"scope", "conversation_ids"}` replaces
  one scope's order and answers with what was kept.
- **Table.** The store keeps one row per user and scope in `conversation_order`
  (migration 095): a JSON array of conversation IDs, first to last. This avoids
  changing the conversations table and its shared column list, and a reorder
  costs one write.
- **The server keeps only what is the caller's.** `store.SetChatOrder` keeps
  only IDs of the caller's own, live conversations that are in the scope
  (pinned, or filed in that project). They are kept in the order given and
  without duplicates. An ID outside that set is dropped rather than refused,
  because a chat can be unpinned or moved in another tab between a drag and its
  save.
- **Limits.** A save that keeps nothing removes the row. Any other scope name
  is a 400. The list is capped at 500 IDs (`store.MaxChatOrder`), and the
  request body at 64 KiB.
- **Clean-up:**
  - deleting a project (`DeleteProject`) removes every member's order for it;
  - deleting an account (`DeleteUser`) removes its orders, and the orders for
    the projects it owned, in the same transaction.

### Ordering rules

- **One ordering rule for the whole rail.** `applyChatOrder` in
  `conversationOrganization.ts` orders:
  - the rendered Pinned section;
  - each project group (`projectGroups`);
  - the keyboard `j`/`k` walk (`visibleConversationOrder`, which also applies
    the Temporary sort), so the rendered rows and the walk cannot drift.
- **New arrivals.** A chat the order does not name sorts above the ordered
  ones. That means one pinned or filed since the last reorder, so it lands
  where the person just looked rather than at the bottom of a long list. IDs
  that have left the scope are ignored on read.
- **Saving.** Moves apply at once and their saves run one after another, so a
  quick run of moves reaches the server in the order it was made. Reads and
  moves share a generation counter, so a slow first read cannot overwrite a
  move made while it was in flight. A refused save re-reads the server's
  orders, so the rail never shows an order that was not kept. The first read
  is best-effort: if it fails, every scope stays most recent first.

## Deliberately not done

- **No hand-set order for Temporary chats.** See above: they get a sort. The
  sort is a per-device preference rather than an account setting. It is a view
  choice and needs no server state.
- **No drag on the ordinary rows.** Ordinary rows already drag into projects.
  Making the same gesture also reorder would make every drag ambiguous, so
  reordering is its own mode.
- **Only your own chats in a project.** The rail lists only your own chats in a
  project (teammates' shared chats live on the project home, ADR-0057), so a
  project's order covers those. Each member orders their own.
- **Unpinning or moving a chat out does not edit the saved order.** The stale
  ID is ignored on read and dropped at the next save. If the chat comes back
  before then, it returns to its old place.

## Tests

- `internal/store/chat_order_test.go`:
  - saving, reading back, replacement, de-duplication and the cap;
  - the ownership and scope filters for pinned and project scopes;
  - empty saves removing the row, bad scopes;
  - clean-up on project and account deletion (Postgres).
- `internal/httpapi/chat_order_test.go`: the GET/PUT route through the
  `/conversations/` dispatcher, a bad scope, bad JSON and a wrong method.
- `conversationOrganization.test.ts`:
  - `applyChatOrder` and `movePinned`;
  - per-project ordering in `projectGroups`;
  - `sortRecent` (each sort, stable ties, a missing `created_at`);
  - that the rail and keyboard orders agree.
- `ChatReorderList.test.tsx`:
  - arrows, ends, drag and drop, ignoring a foreign drag;
  - Done and `Esc`, and that `Esc` is not seen by the rail;
  - focus on entry and after a move.
- `ConversationSidebar.chatOrder.test.tsx`:
  - when Reorder is offered, including the 500 cap;
  - focus on exit;
  - project reorder from the menu, in the project's scope;
  - the Temporary sort.
- `web/e2e/mocked/chat-order.spec.ts`:
  - the saved order on screen, and arrow and real-drag moves with their PUTs;
  - a slow first read not undoing a move;
  - no mode for a viewer;
  - `Esc` leaving the mode without closing the narrow-screen rail overlay;
  - project reorder from its menu, with focus returned;
  - the Temporary sort, remembered across a reload;
  - axe in both themes with the mode open.
