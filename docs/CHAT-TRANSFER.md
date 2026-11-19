# Transferring a chat to another person

A chat, with its dashboard and its patrols, can be handed to another person:
the owner picks **Transfer ownership…** on the chat's row menu, names the new
owner by email, and the chat is theirs. It exists for work that is built by
one person for another, such as a patrol and dashboard set up for a client and
handed over when it is ready, where rebuilding it under the new owner's
account would mean redoing the transcript, the workspace files and the patrol.

User-facing behaviour is in the chat guide
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`, "Sharing with
your team"), which `/help` renders.

## What shipped

**`POST /conversations/{id}/transfer`** with `{"to_email": "..."}`
(`internal/httpapi/conversation_transfer.go`; browser route
`POST /api/conversations/{id}/transfer`). The caller must be the chat's owner
or an admin; anyone else gets the same 404 as for a chat that does not exist.
The transfer is direct. There is no offer for the new owner to accept.

The new owner must have an enabled account that is not a read-only viewer.
They can be on any team, or none. An archived chat or a Lockdown chat is
refused (409), and the menu does not offer the item on one; a target that
cannot take the chat is refused (400) with one message whatever the reason.
A chat that is not idle is refused (409) until it is: a turn running in it,
or a turn or approval that still holds its workspace. A participant's turn has
the owner's private workspace subtrees swapped out, and moving the chat under
one could hand them to the new owner when the turn puts them back. Idle is
made to hold for the whole transfer:

- `beginTransfer` refuses while a turn is running, an approval is being
  decided or an approved action is being executed, and marks the chat as
  changing hands under the lock turn registration takes, so no turn can
  register until the transfer has finished (`registerTurnGated`). A decision
  on an approval card (approve, reject, an expiry noticed on click, or a
  dismissal) is admitted under that same lock and counted until it returns
  (`beginApprovalDecision`); one that arrives while the chat is marked is
  refused (409) and the card stays pending.
- The handler then takes the conversation's workspace lock, the one a turn or
  an approval being executed holds from preparing the workspace until it has
  put it back, waiting up to 10 seconds for it.
- Under that lock it first finishes any participant workspace swap a crashed
  process left behind (`recoverInterruptedParticipantWorkspace`, as every turn
  does before it starts). If that fails, the transfer does not happen.

**What `store.TransferConversation` does, in one transaction:**

| | |
|---|---|
| Owner | `conversations.user_email` becomes the new owner. |
| Audience | The chat becomes private to the new owner: team sharing (`team_visible` and the team stamp), Multiplayer mode with its participant and exclusion lists, and the public link are cleared. Who can see a chat is its owner's decision, so the previous owner's audience does not carry over. |
| Owner's arrangement | Pin, labels, project filing and connector account choices (`mcp_accounts`) are reset. |
| Approval mode | Back to Manual (`auto_approve` off). Auto-approve and Autonomous are each owner's own choice, and Autonomous needs that person's acceptance. |
| Approval timeout | The chat's own timeout override is cleared, so the new owner's approval cards use the default. |
| Patrols and timers | The previous owner's active and paused schedules in the chat move to the new owner and are **paused**, as are any the new owner already had in the chat as a participant; a fire in flight is revoked. A patrol runs with its owner's connectors, so it runs again only when the new owner resumes it. |
| Queue | Every queued input in the chat is cancelled. |

The transcript, the workspace files (attachments included), the persona, the
model and the enabled optional connectors are untouched.

**Before the transaction** the handler removes the previous owner's personal
skills from the workspace (`user-skills/`) and from the stash a participant's
turn moves them to. If that fails, the transfer does not happen: the skills
must be gone before the chat is anyone else's. Nothing is lost if the transfer
is then refused, because skills are written into the workspace again at the
start of each of their owner's turns.

**After the transaction** (on a context detached from the request, so a
client that has gone away does not skip it) the handler:

- bumps the conversation's ownership epoch. A message the previous owner
  submitted just before the transfer was admitted while they still owned the
  chat; `registerTurnGated` compares the epoch under the lock the transfer
  bumped it under and refuses to start that turn. A later submission from
  them is re-checked against the store and turned away.
- reconciles the chat with its new, private audience the same way a removed
  Multiplayer participant is reconciled (`enforceMultiplayerRevocation`): a
  turn the previous owner or a participant still has running is stopped, their
  pending approvals, session approvals and their own patrols in the chat are
  cancelled, and their live streams are detached.
- cancels pending auto-submit countdowns.
- releases the chat's persistent sandbox, where the deployment keeps one
  (`FLEET_PYTHON_REPL_MODE=persistent`): the previous owner's live Python
  state was never part of the transcript or the workspace, and the new
  owner's first turn starts a fresh one.
- moves the attachments the new owner uploaded while they were a participant
  out of the participant stash and into the workspace, where an owner's
  attachments live, so links from their earlier turns keep resolving. The
  two can be on different volumes (they are in the Helm chart), so an entry
  that cannot be renamed across is copied and then removed.

A workspace file download that passed its access check just before the
transfer is checked again once it holds the workspace swap lock (the lock the
attachment move above runs under), for any chat that has changed owner since
the process started (`handleWorkspaceFile`), so it serves by who owns the
chat now.

The transfer is logged (`conversations: chat transferred`, with the actor,
both owners and the patrol counts).

**Web.** `TransferChatDialog` (`web/src/app/chat/ui/TransferChatDialog.tsx`):
**Transfer ownership…** on the owner's chat row menu, and on a Team chats row
for an admin. The dialog says what goes with the chat and what the person
giving it away loses, takes the email, and after the transfer says how many
patrols moved. The chat then leaves the previous owner's lists.

**Tests.** `TestTransferConversation`, `TestTransferConversationRefusals`
(store, against Postgres), `TestConversationTransferRoute` (who may ask, the
refusals, the cleanup), and `web/e2e/mocked/transfer-chat.spec.ts` (the menu
item, the dialog's copy, a refusal, the request, the row leaving, the admin
entry, axe in both themes).

## Deviations from the ask

- **The entry is on the owner's own row menu, and on Team chats rows for
  admins only.** A Team chats row is someone else's chat, so only an admin can
  move it from there.
- **Patrols arrive paused rather than running.** A patrol written by one
  person would otherwise start running on another person's connectors the
  moment the chat moved. Resuming is one click in the Patrol bar.

## Honest scope: what this does not do

- **No acceptance step and no undo.** The new owner is not asked. To give a
  chat back, the new owner transfers it again.
- **The previous owner keeps no access.** The new owner can turn Multiplayer
  on and add them.
- **The new owner gets the whole chat.** The assistant works from all of its
  history on their turns, including what earlier turns fetched and ran. On
  screen, the tool details of turns in a chat that was not Multiplayer show
  as the owner's (those turns carry no sender); in a chat that was
  Multiplayer, earlier turns keep their senders and show as a teammate's.
- **A chat on one of the previous owner's own personas** keeps the reference,
  which does not resolve for the new owner: its turns run on the default
  persona until the new owner picks one in a new chat or recreates it.
- **Connectors are the new owner's.** A patrol that used a connector or an
  account only the previous owner has will fail or ask until the new owner
  connects it.
- **Usage and cost already recorded stay with whoever ran the turns.**
- **No transfer of projects' chats in bulk, and no admin screen** listing
  chats to move: an admin transfers a chat they can see under Team chats.
