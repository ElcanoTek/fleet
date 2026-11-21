# ADR-0117: Helping a chat's people — who needs help, a read-only view, pinned notes

- **Status:** Accepted
- **Date:** 2026-11-21
- **Deciders:** fleet maintainers
- **Extends:** [ADR-0115](0115-chat-administration.md)

## Context

ADR-0115 gave administrators the owner's actions in any chat: read it, send a
message, stop it, transfer it, edit its patrols. Using it showed three gaps.

- **The page started from chats, not people.** Settings → Admin → Chats was one
  list of every chat, each expanding into every form at once. Some people need
  more help than others, and nothing said who: an administrator had to open
  chats to find the one where someone had been waiting three hours on an
  approval card.
- **Reading a chat meant reading a log.** The transcript was a column of
  labelled plain-text entries. An administrator helping someone could not see
  what that person was looking at: their own words, the assistant's formatted
  reply, the table in it.
- **The only way to say something to the person was through the model.** A
  message starts a turn, spends the owner's budget and is addressed to the
  assistant. A notice is one line that scrolls away. There was no way to leave
  the *person* a note ("the report is ready; approve the card under the last
  reply") that the model does not read.

One thing was asked for and deliberately not built: letting an administrator
open someone's chat in the chat view and use it as their own. Typing as the
owner would make the owner's words and the administrator's indistinguishable,
which is the property ADR-0115's labelled admin message exists to keep.

## Decision

### Start from who needs help

- **`GET /admin/chat-overview`** (store `AdminChatOverview`) counts the chats a
  filter covers, as a whole and per owner: chats, needing attention, running,
  with an active patrol, with a pinned note, and approval cards waiting; plus
  how many are archived and in the recycle bin. Owners with chats needing
  attention come first.
- **A chat needs attention** when an approval card is waiting in it, or its
  latest turn ended in an error. Nothing else. Both are things its owner is
  stuck behind that an administrator can see without reading the transcript,
  and both are exact (a row in `chat.approvals`, the status of the latest row
  in `chat.turns`). `AdminChat` carries the reasons (`attention`), when the
  oldest waiting card was staged (`waiting_since`) and the latest turn's status.
- **`GET /admin/chats?state=`** narrows a listing to `attention`, `running`,
  `patrols`, `noted` or `archived`; its search now matches the owner's email as
  well as the title and id.
- The page opens on four counts that filter it and the chats grouped by person.
  A flat list, archived chats and the recycle bin are one click away.

### A view, not a seat

One chat opens on its own (`/settings/admin/chats?chat=<id>`): what it is
waiting on (each waiting card's tool, for whom, for how long, and when it is
denied unanswered), then the conversation laid out **as its owner sees it** —
their messages as bubbles, replies as formatted text, patrol runs,
administrator messages and notices in the forms the chat gives them — with
tool calls on request and earlier messages paged in (`?before=`).

- It is **read-only**. There is no composer, and it does not appear in the
  administrator's own chat list.
- **Files stay with the owner.** The workspace route is the owner's, so a file
  or image the conversation points at is named, never fetched.
- **Answering an approval card stays with the chat's people** (ADR-0115,
  unchanged). The page says so where it shows the waiting card.
- It re-reads the chat every 8 seconds while its tab is visible.

Stepping in happens in one panel below the conversation, which says for each
way who sees it and whether the assistant reads it: pin a note, message the
assistant (ADR-0115's admin message, unchanged), or add a notice.

### A note pinned to the chat

An administrator can pin **one note** to a chat.

- **Where it lives.** `chat.admin_notes` (migration 0209), one row per chat,
  and *not* an entry in `chat.messages`. The model's context is built from
  `chat.messages`, so a note kept outside it cannot reach the model, be
  folded into a compaction summary, be copied by a branch or appear in an
  export. That is a property of where the row is, not of a filter that has to
  be remembered at each read.
- **Who sees it.** Everyone who can open the chat through the conversation
  route: its owner and its Multiplayer participants. It rides the conversation
  payload (`admin_note`) and renders above the conversation, titled *Note from
  an administrator* with the administrator's email. A teammate reading a
  team-shared chat and a reader of a public share link do not get it: those
  snapshots are built from the history.
- **It says it is not a message.** The banner states that the assistant does
  not read it and that a reply typed in the chat will not reach the
  administrator.
- **Acknowledging it.** *Got it* (`POST /conversations/{id}/admin-note`, with
  the `updated_at` of the text that was read) records the first person who
  pressed it, and when. The banner then folds to one line; it stays pinned
  until an administrator removes it. Replacing the text clears the mark, and an
  acknowledgement of text that has since been replaced marks nothing.
- **Setting and removing it.** `PUT` / `DELETE /admin/chats/{id}/note`,
  `fleet chats note`. At most 2000 characters. Allowed on an archived or
  Lockdown chat (nothing runs); refused on a chat in the recycle bin.
- **Accountability.** Recorded in `chat.admin_actions` as `note_set`,
  `note_replace` or `note_clear`, with the text. It takes **no reason** and
  writes **no notice** into the conversation: the note is itself what the
  chat's people are shown, with its author's name on it. This is the one
  deliberate exception to ADR-0115's "every change takes a reason and is
  announced with a notice"; a notice (ADR-0115) is the other change that takes
  no reason, for the same cause.
- **Lifetime.** It goes with the chat when the chat is deleted for good
  (foreign key, cascade) and stays with a chat in the recycle bin, so a
  restore gives it back.

### The same from a terminal

`fleet chats people`, `fleet chats list --attention` / `--state`,
`fleet chats note` (show, pin, replace, clear), `fleet chats history --before`,
and `fleet chats show` with the attention reasons, the waiting cards and the
note. Each prints JSON with `--json`, so the operations agent that runs a box
can find who is stuck and leave them a note without a browser.

## Consequences

- An administrator can now leave words in someone's chat that cost nothing to
  run and that the model cannot act on. The note names its author and is
  recorded, which is how its use stays accountable.
- "Seen" is one mark per note, not one per person. In a Multiplayer chat it
  says someone read it, not that everyone did.
- A chat whose latest turn failed stays in "needs attention" until its next
  turn, or until that turn's record is swept: finished turns are deleted after
  `FLEET_TURN_EVENT_RETENTION_DAYS` (14 days by default), so a chat abandoned
  after a failure drops out of "needs attention" then. Nothing is stored to
  outlast the sweep on purpose: a failure nobody has come back to in two weeks
  is not something an administrator can usefully act on, and keeping one turn
  (with its event ledger) per chat for ever to say so is the wrong trade.
  There is no way to dismiss it earlier; the count is of facts, not of a to-do
  list. An install with many failed chats narrows the page with "active in the
  last N days".
- An open chat tab does not show a newly pinned note until its next load or
  turn, as with every admin change (ADR-0115's deferral, unchanged).
- The overview runs two aggregate queries over the chats in scope, each with
  per-chat sub-selects on indexed columns. It is read on page load and every
  30 seconds while the page is visible.
- The down migration drops `chat.admin_notes`; pinned notes are lost with it.
