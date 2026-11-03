# Dashboard view: the chat as a live board

Dashboard view is a full-screen layout for a chat conversation. The agent's
display cards stay on screen as **panels** on a board. The ordinary chat
(transcript, composer, approval cards, forms) narrows to a rail beside the
board. It is the "advanced view" of a chat. You still type, attach, approve
and send email exactly as before, but the numbers you are watching no longer
scroll away. The view is general-purpose: nothing in it knows about trading,
deals or any other domain.

User-facing behaviour is in the chat guide
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`, "Dashboard
view"), which `/help` renders.

## What shipped

**A panel is a show_ui card.** There is no new component catalog, table, SSE
event or endpoint. The board is derived from the transcript, the same way card
state already is (`web/src/app/chat/ui/dashboard/board.ts`), so a reload, a
second tab and a teammate's multiplayer view all build the same panels. A card
lands in a slot as follows:

1. A card with a `panel` key (below) lives in slot `p:<key>`. A later card with
   the same key takes the slot over, and the panel shows `v2`, `v3`, … and
   flashes once.
2. A card whose `replaces` names a card on the board inherits that card's slot.
3. A display-only card (no inputs, no buttons), or a card the user pinned, gets
   a slot of its own (`c:<card id>`).
4. Anything else (a form without a panel key) stays inline in the chat rail. A
   form is a step in the conversation, not something to watch. **Pin to board**
   under it adds it as a panel.

A card on the board is drawn once, on the board (`GenerativeCard
variant="panel"`). The rail shows a one-line pointer that jumps to the panel,
so no card is ever live in two places.

**`panel`: a new optional top-level key on `show_ui`.** `genui.Validate`
accepts it when it is 1–40 characters of `a-z 0-9 - _` and refuses anything
else with a fixable error. It is also allowed beside `from_result`. The tool
description has a short DASHBOARD PANELS section, and the acknowledgement names
the panel and the key to reuse. In normal view a same-key card supersedes the
earlier one like `replaces` does (`deriveGenUiState`). See
[GENERATIVE-UI.md](GENERATIVE-UI.md#dashboard-panels-panel).

**`view: "dashboard"`: a per-turn hint.** While the view is on, the web client
sends `view: "dashboard"` on `POST /api/chat`. The handler then appends one
note to the turn's injected context (`appendDashboardViewBlock`,
`internal/httpapi/chat.go`). The note tells the agent that the user is reading
a board, to put watched figures in panel cards, to refresh a panel instead of
restating it, and to keep prose short. The request also carries `board`: the panels on the
sender's board, in order, as `{title, card_id, panel?, pinned?}`. The note
lists them one per line with how to update each one: `panel "<key>"` for a
keyed panel, `replaces="<card_id>"` for a card without a key (typically one the
user pinned). So "update this chart" or "refresh the second panel" resolves to
a card. The list is bounded (24 panels, titles cut to 80 characters) and
sanitized: control characters become spaces, square brackets are dropped so a
title cannot close the note, and an entry with an unusable card id or panel key
is skipped. Like every injected block it is persisted with the user message, so
the transcript shows exactly what the model was told. Any `view` other than
`"dashboard"` adds nothing, and `board` is ignored without it.

**Pin, then update.** A pinned card keeps its slot when the agent refreshes it
with `replaces`. If the refresh also gives it a `panel` key, the slot moves to
that key and keeps its place, version count and the user's width, order and
removal choices (`deriveBoard` carries the old key as an alias), so later
updates can simply reuse the key. If the key already belongs to another panel,
the refined card's old slot merges into that panel rather than staying on the
board. Refining a version that is no longer current takes nothing over, which
matches how the transcript decides what is superseded.

**Moving between the views.** Toggling never remounts the transcript or
composer: the layout wrapper is always rendered (`display: contents` in normal
view) and the conversation rail is hidden, not unmounted. Composer drafts, card
drafts (stored under the same key whether the card is drawn on the board or in
the transcript) and queued inputs survive any number of round
trips, and leaving closes any focused panel and the phone sidebar drawer.
The conversation-list shortcuts (`j`/`k`, `Enter`, `P`/`A`/`R`/`#`) are off
while the rail is hidden, and `⌘K` leaves dashboard view so the search it
focuses is on screen. Focusing a panel moves the keyboard focus into it, and
closing returns it to that panel's Focus button.

**The screen.**

- **HUD bar** in place of the page header: the conversation title, a status
  light (**Standby**; the running tool while a turn works; **N approvals
  waiting**, which jumps to the first approval card), the panel count, a clock,
  a **How it works** popover with the instructions, and **Exit** (`Esc`).
- **Board**: a dot-grid canvas of panels with HUD-style corner brackets.
  Charts, tables and grids default to two columns wide, everything else to one.
  A panel can be widened or narrowed, focused to fill the board, reordered (drag
  the grip, or focus the grip and use the arrow keys), or removed. A removed
  panel's card returns to the chat. The empty board shows three starter
  requests, which fill the composer and do not send.
- **Rail**: the existing transcript and composer, unchanged. Approval cards,
  email previews and forms render inline as usual. The conversation sidebar
  stays mounted but hidden.
- **Entry points**: **Dashboard** in the chat header, **Dashboard view** in the
  composer's **More options** menu, and the `D` shortcut (listed in the `?`
  overlay and the guide's shortcut table).
- **Phones**: the HUD shows **Board / Chat** buttons and one pane at a time.
  The status light keeps its LED and count, with the words for screen readers
  only.
- **Themes and motion**: tokens only (`globals.css`, `.dash-*` in
  `@layer components`), so a client palette and light/dark repaint it. Every
  animation stops under `prefers-reduced-motion`.

**Per-browser layout.** Whether a conversation opens in dashboard view, and its
board layout (pins, removals, order, widths), are kept in `localStorage`
(`fleet.dashboard.view.v1`, `fleet.dashboard.board.v2`). Each list is capped at
the 60 most recent conversations, and every access is wrapped so blocked
storage degrades to "forgets on reload". A draft keeps its view when it gets
its id. A new chat started from dashboard view stays in it.

**Tests.** Go: the `cards.json` fixture cases (a valid panel card, a non-slug
key, a non-string key), `TestShowUIPanelCard`, `TestShowUIRef_Panel`, and
`TestAppendDashboardViewBlock`, `TestDashboardBoardIsListed` (keys, pinned cards,
sanitizing, the bound) and `TestChatRequestDecodesView`. Web: `board.test.ts`
(slotting, lineage, pins, the slot moving to a new key, removal, order, width,
reordering, status, the manifest),
`prefs.test.ts` (round-trip, bounds, throwing storage), and `transcript.test.ts`
(same-panel supersede, refused keys). Mocked Playwright
(`e2e/mocked/dashboard-view.spec.ts`) covers the desktop board and rail, pin and
remove, the `view` and `board` fields on the POST, three round trips between the
views (composer draft, card draft, full cards back in the transcript), keyboard reorder, focus and `Esc`,
remembered layout, the phone panes, and axe over the HUD and board in both
themes.

## Deviations from the ask

- **Panels are agent-built cards, not a fixed set of chart widgets.** A
  trading-terminal layout with hard-wired tiles would be domain-specific. The
  board instead shows whatever the agent puts in panel cards, using the
  existing catalog (`stat`, `chart`, `table`, `status_list`, `progress`, …).
  That keeps it general-purpose, keeps cards declarative data (ADR-0080) and
  adds no new rendering surface to secure.
- **Layout is per browser, not per conversation.** Storing it on the
  conversation would need a migration and a write path, and it would impose one
  person's arrangement on every reader of a shared chat. The panels themselves
  are shared because they come from the transcript.

## Honest scope: what this does not do

- **No live data feeds.** A panel changes only when the agent shows a new card
  for it: on your message, a card submission, or an
  [in-chat patrol](CHAT-PATROLS.md) turn. Nothing in the browser polls a data
  source. A patrol that re-shows a panel is how a board "ticks".
- **Steers carry no note.** A message queued behind a running turn
  (`mode: "queue"`) keeps its `view` and `board` on the queue row
  (`chat_input_queue.view_context`, migration 094) and drains with the same
  note as a direct send. A *steer* is injected into the running turn as text
  and carries none (that turn already had the note if it was sent from
  dashboard view), and patrol and approval-resume turns carry none either.
- **The board list is the sender's.** In a multiplayer chat each participant's
  board is their own arrangement, so "the second panel" means the second panel
  on the board of whoever sent the message.
- **The agent may ignore the hint.** It is an instruction, not a constraint. A
  model that never uses `panel` still gets a working board, because display-only
  cards are placed automatically. Its refreshes then arrive as new panels
  rather than updates, unless it uses `replaces`.
- **No timestamps on panels.** Messages carry no client-visible time, so a
  panel shows its version count, not "updated 2 min ago".
- **No masonry.** Panels fill the grid in board order (not `dense`), so what
  the user sees second is the panel the agent is told is second, and keyboard
  order matches. Mixed widths and heights can leave gaps.
- **Web chat only.** The terminal client and ACP editors see `show_ui` calls as
  before. The shared read-only transcript does not offer the view.
- **Panels from before a compaction are archived.** They render read-only with
  an **archived** tag. The model no longer has their definitions, so ask it to
  show them again.
