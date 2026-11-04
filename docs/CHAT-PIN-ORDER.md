# Pinned-chat order in the rail

People with many pinned chats asked to put them in their own order rather than
most-recent-first. This note records what shipped, how it works, and what was
left out on purpose.

## What shipped

- **Reorder mode.** With two or more pinned chats, the rail's **Pinned**
  heading shows **Reorder**. It swaps the ordinary rows for a list
  (`PinnedReorderList.tsx`). Each item can be dragged, or moved one place with
  its up/down buttons. The buttons are how the list works from the keyboard and
  on touch screens, where HTML drag and drop does not fire. Each move is
  announced through a polite live region. **Done** or `Esc` leaves the mode.
  `Esc` is taken in the capture phase and stopped there, so on a narrow
  screen it does not also collapse the rail overlay or close the sidebar.
  Entering the mode moves the focus to the list's first control. After an
  arrow move, focus stays on that chat's arrow, or moves to its other arrow
  when the first one is now disabled at an end. Leaving the mode returns the
  focus to **Reorder**.
- **Who is offered it.** The rail offers the mode for 2 to 500 pinned chats;
  500 is the server's cap. It is not offered to a read-only `viewer` account,
  whose writes are refused (the role comes from `GET /api/me`).
  The drag uses its own MIME type, so a reorder drag is never taken for the
  existing drag of a chat into a project.
- **Saved per user, on the server.** `GET /api/conversations/pin-order` reads
  the order and `PUT` replaces it with `{"conversation_ids": [...]}`. The order
  follows a person across devices. The store keeps one row per user in
  `conversation_pin_order` (migration 095): a JSON array of conversation IDs,
  first to last. This avoids changing the conversations table and its shared
  column list, and a reorder costs one write.
- **The server keeps only what is the caller's.** `store.SetPinOrder` keeps only
  IDs of the caller's own, live, pinned conversations, in the order given and
  without duplicates. It answers with what it kept. A foreign, unpinned,
  deleted or unknown ID is dropped rather than refused, because a chat can be
  unpinned in another tab between a drag and its save. The list is capped at
  500 IDs (`store.MaxPinOrder`), and the request body at 64 KiB. Deleting an
  account (`Store.DeleteUser`) removes its saved order in the same
  transaction.
- **One ordering rule for the whole rail.** `orderPinned` in
  `conversationOrganization.ts` is used by both the rendered Pinned section and
  the keyboard `j`/`k` order (`visibleConversationOrder`), so the two cannot
  drift. A pinned chat the order does not name, meaning one pinned since the
  last reorder, sorts above the ordered ones. A fresh pin then lands where the
  person just looked rather than at the bottom of a long list. IDs in the order
  that are no longer pinned are ignored on read.
- **Saving.** Moves apply at once and their saves run one after another, so a
  quick run of moves reaches the server in the order it was made. Reads and
  moves share a generation counter, so a slow first read cannot overwrite a
  move made while it was in flight. A refused
  save re-reads the server's order, so the rail never shows an order that was
  not kept. The first read is best-effort: if it fails, pinned chats stay
  newest first.

## Deliberately not done

- **Temporary chats stay newest first.** That list is defined by recency and
  expiry; a hand-placed row there would be pushed around by every new message.
  To give a chat a place of its own, pin it, as the guide says.
- **Project chats keep the project's order.** Ordering inside projects would
  need a per-project order and was not asked for.
- **No drag on the ordinary rows.** Ordinary rows already drag into projects.
  Making the same gesture also reorder would make every drag ambiguous, so
  reordering is its own mode.
- **Unpinning does not edit the saved order.** The stale ID is ignored on read
  and dropped at the next save. If the chat is pinned again before then, it
  returns to its old place.

## Tests

- `internal/store/pin_order_test.go`: saving, reading back, the ownership and
  pinned filter, de-duplication, replacement and the cap (Postgres).
- `internal/httpapi/pin_order_test.go`: the GET/PUT route through the
  `/conversations/` dispatcher, bad JSON and a wrong method.
- `conversationOrganization.test.ts`: `orderPinned`, `movePinned`, and that the
  rail and keyboard orders agree.
- `PinnedReorderList.test.tsx` and `ConversationSidebar.pinOrder.test.tsx`:
  arrows, ends, drag and drop, ignoring a foreign drag, Done and `Esc` (and
  that `Esc` is not seen by the rail), focus on entry, after a move and on
  exit, and when the mode is offered (including the 500 cap).
- `web/e2e/mocked/pin-order.spec.ts`: the saved order on screen, arrow and real
  drag moves with their PUTs, a slow first read not undoing a move, no mode
  for a viewer, `Esc` leaving the mode without closing the narrow-screen rail
  overlay, and axe in both themes with the mode open.
- `TestPinOrder_GoesWithTheAccount`: account deletion purges the saved order.
