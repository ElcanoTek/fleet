# ADR-0115: Chat administration

- **Status:** Accepted
- **Date:** 2026-11-20
- **Deciders:** fleet maintainers

## Context

In the app, every chat belongs to its owner (and the participants they invite).
The routes that change a chat are owner-scoped: only the owner can rename it,
send it a message, stop it, pause or edit its patrols. Transfer is the one
exception: an administrator can already transfer someone else's chat.

Operators keep needing to look after other people's chats. An example from one
client server, after ADR-0114 shipped the shared team log:

- The client's main optimization chat ran a 06:30 patrol whose prompt still told
  the assistant to keep its log in the chat's own folder and to offer a
  Save-memory card for every change.
- Its owner had just changed, and the person who maintained the patrol was not
  its owner.
- The only way to fix the prompt was a hand-written `UPDATE` on
  `sched.chat_schedules`. That skips the `def_version` bump the scheduler
  relies on, keeps no backup, leaves no trace in the chat, and can overwrite an
  edit the owner made after the operator looked.

Sending such a chat a message ("publish the team log now"), stopping a runaway
turn, or reading what happened had no operator path at all.

We expect to do this regularly. It needs one safe tool that the on-box operator
(and the operations agent that runs our boxes) can script, and the same powers
in the browser for administrators.

## Decision

Add chat administration, with one store layer and two ways in:

- **`fleet chats`** on the host (`list`, `show`, `history`, `send`, `stop`,
  `transfer`, `notice`, `set`, `actions`, `request`).
- **`fleet patrol`** on the host (`list`, `show`, `set-prompt`, `pause`,
  `resume`).
- **Settings → Admin → Chats** in the browser, backed by the admin-only routes
  `/admin/chats`, `/admin/chats/{id}[/history|/requests|/notice]`,
  `/admin/patrols/{id}`, `/admin/chat-requests/{id}` and `/admin/chat-actions`.

### What an administrator can do

- **Find and read any chat.** List chats across every user, filtered by owner,
  title or id, and recent activity. Each chat shows its owner, model, approval
  mode, patrols, waiting approval cards, queued inputs and running turn. The
  transcript shows its latest entries, optionally with tool calls.
- **Send a message.** It becomes a queue row of a new mode, `admin`
  (migration 0207):
  - The row's user is the chat's **owner**, so the turn runs with the owner's
    connectors and approval mode, exactly like a patrol run.
  - The row's new `sender_email` column holds the **administrator**. The turn
    records them as the message's sender (`TextContent.SenderEmail`), and the
    input kind `admin_message` makes the chat show it as an administrator's
    message, not the owner's bubble.
  - The model receives the words under a header naming the administrator, and a
    footer saying the owner did not write them.
  - Any approval card the turn stages goes to the owner. Administrators cannot
    answer someone else's cards.
  - Like the other fleet-started kinds, an admin turn counts as unattended:
    there is no model-upgrade suggestion, no memory auto-index and no
    escalation classifier.
- **Stop a chat.** This is the owner's Stop (scope all): the running turn, the
  queued inputs and the auto-submits.
- **Transfer a chat.** This is the owner's transfer, with the same refusals and
  the same clean-up afterwards. The handler's logic now lives in
  `transferConversation`, which both paths call.
- **Change settings.** Title, model, approval mode and archive state:
  - The approval mode may only be set to **manual** or **auto**. Autonomous
    runs SSP writes without a card and needs the owner's own acceptance, so an
    administrator can never turn it on.
  - A Lockdown chat keeps its model.
- **Post a notice.** A UI-only note in the chat that the model never sees.
- **Edit a patrol.**
  - Its instructions can be changed with a compare-and-swap on the prompt's
    sha256, so an edit built from text the owner has since changed is refused.
  - The change bumps `def_version`, exactly like the owner's own edit, so a run
    already claimed under the old prompt is superseded.
  - The CLI shows the diff first and backs the old prompt up to
    `/var/backups/fleet/patrol-prompts/`.
  - Patrols can also be paused or resumed. That goes through the owner's own
    path, which revokes a run in flight and cancels the patrol's queued turns.

### Every change is accountable

Every change takes a reason, is recorded in `chat.admin_actions` (who, through
`cli` or `web`, what, which chat or patrol, why), and is announced in the chat
with a UI-only notice. The CLI records the actor given with `--as` (default
`<unix user>@<host>`); the browser records the signed-in administrator.

### What the running server must do

A message, a stop and a transfer need the running server: a turn, a running
turn's cancellation, a transfer's clean-up. A message also needs the input
queue's in-memory acceptance order (the counter Stop compares against).

So the CLI, which talks to the database and not to the server, records these
three as rows in `chat.admin_requests`. The server carries them out on its next
scheduler pass (`KickQueuedChatScheduleTurns`, every `FLEET_SCHED_TICK_SECONDS`,
30 s by default), with the same code its own routes use:

- A message is queued under the request's row lock, in the same transaction
  that marks the request done, so it is queued exactly once.
- The browser path records the same request and executes it at once.
- `fleet chats send --wait` follows the request to the end of its turn and
  prints the reply.

The sweep that restarts queued patrol rows after a restart now also restarts
queued `admin` rows: neither has a client waiting to submit again.

## Consequences

- Operators stop writing SQL against chat and schedule tables. The README's
  guardrail points at `fleet chats` / `fleet patrol`.
- An administrator's message spends the owner's connectors and model budget,
  as a patrol does. The audit table and the notice in the chat are how the
  owner sees that it happened.
- What an administrator can do is the owner's set of actions minus three:
  answering approval cards, turning on Autonomous mode, and deleting the chat.
  Deleting stays the owner's (or the retention sweep's).
- `chat.admin_actions` and `chat.admin_requests` grow with use. They are small
  (one row per change), and are covered by the nightly backup.
- The down migration drops both tables, deletes `admin` queue rows that never
  ran, and narrows the queue's mode check again.
