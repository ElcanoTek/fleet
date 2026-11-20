# ADR-0116: Administrators can delete chats, through a recycle bin

- **Status:** Accepted
- **Date:** 2026-11-20
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0115](0115-chat-administration.md)

## Context

ADR-0115 deliberately left deleting a chat to its owner. Two needs since then:

- **A chat that should not exist.** For example, a user forked a chat by
  mistake instead of joining the Multiplayer one, and the operator needs it
  gone without waiting for that user.
- **A box running out of room.** It may hold many chats nobody uses, and the
  operator must be able to free space now.

Archive is the safer tool and stays the default: nothing is lost. But both
cases need delete, and only one of them needs it to be immediate and
irreversible.

Fleet already deletes a chat in one of two ways, chosen by
`FLEET_CONVERSATION_SOFT_DELETE` (off by default):

- **Hard delete.** The row goes now.
- **Soft delete.** The row is tombstoned (`deleted_at`), every read already
  skips it, and `SweepExpired` purges it after 30 days. There was no way to
  restore one.

## Decision

Administrators get three more admin requests, carried out by the running
server like a stop or a transfer (ADR-0115; migration 0208 widens the kinds):

- **`delete`: to the recycle bin.**
  - It stops the chat's work. Then, in one transaction under the chat's lock,
    it pauses every patrol and timer in the chat (whoever owns it), cancels
    the queued inputs and tombstones the chat.
  - Nothing else is taken apart: the pin, sharing, participants, their paused
    patrols, pending cards and private attachments all stay, so a restore
    gives the chat back whole. A tombstoned chat is already unreachable to
    everyone, so nothing needs revoking.
  - Only what lives in the server process is released: the sandbox and the
    session approvals.
  - The owner cannot hard-delete a chat out of the bin.
  - An administrator's delete **always** tombstones, whatever
    `FLEET_CONVERSATION_SOFT_DELETE` says, so it can be undone for 30 days.
  - The existing tombstone purge removes it after that. The bin's window is
    `store.AdminRecycleBinDays`, the same 30 days.
- **`restore`: out of the recycle bin.** The tombstone is cleared and a notice
  says so. The chat's patrols stay paused until the owner or an administrator
  resumes them.
- **`purge`: permanent, now.** This is the emergency path.
  - The chat's row goes first, with its content by cascade. Only then is
    anything else removed: participants' turns and streams, their private
    attachments, the sandbox. A failed delete leaves the chat untouched.
  - The workspace is reclaimed by the orphan sweep, which starts at once.
  - The browser requires typing the chat's title (its id, if it has none).
    The CLI requires `--permanent --yes`.
  - The record in `chat.admin_actions` survives the chat.

The recycle bin is listed with `fleet chats list --deleted` and with *Show the
recycle bin* on Settings → Admin → Chats.

Every delete takes a reason and is recorded, like every admin change. The note
"moved to the recycle bin" is written into the chat before it is tombstoned, so
it is there if the chat is restored.

## Consequences

- An owner's own delete is unchanged: it still follows
  `FLEET_CONVERSATION_SOFT_DELETE`. Restore is administrator-only.
- A binned chat still takes its disk space until the purge. When space is the
  problem, `purge` (or Settings → Admin → Server's storage clean-up) is the
  tool.
- The model never gets a delete tool. These are actions a person takes in the
  admin tools.
