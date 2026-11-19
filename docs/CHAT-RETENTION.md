# Temporary chats: what is deleted, when, and what people are told first

Temporary chats exist for one reason: so a box does not fill up with
conversations nobody will open again. This note records the policy that does
that, and how a person finds out before one of their chats goes.

## What was wrong

A temporary chat was deleted after **14 days** without activity, and a second
rule deleted a person's oldest temporary chats as soon as they had more than
**50**. Three things made that harsher than it needed to be:

- **14 days is shorter than ordinary working rhythms.** A chat used for a
  monthly report, or one someone meant to come back to after two weeks away,
  was already gone.
- **The cap was silent and usually hit first.** Someone who starts four chats
  a day reaches 50 in under two weeks, so their chats were being deleted by
  the cap, with no warning of any kind, before the 14 days ever ran out.
- **The only notice was a mark that never changed.** Every temporary chat
  carried the same hourglass from its first minute to its last, and one line
  of help text behind an ⓘ. Nothing said *when*, so in practice a chat was
  simply missing one day.

## The policy now

What counts as temporary has not changed, and still has one definition
(`store.temporaryConversationClause`, mirrored by `isTemporaryChat` in
`web/src/app/chat/ui/deleteAllUnpinnedScope.ts`): a chat that is unpinned, in
no project, not archived, not shared by link or with the team, not
Multiplayer, and has no armed or paused patrol or timer.

| Rule | Before | Now | Setting |
| --- | --- | --- | --- |
| Deleted after this long without activity | 14 days | **90 days** | `CONVERSATION_TTL_DAYS` |
| Flagged in the interface this long before | never | **the last 14 days** | `FLEET_CONVERSATION_EXPIRY_WARNING_DAYS` (0 = off) |
| Most temporary chats one person keeps | 50 | **500** | `CONVERSATION_UNPINNED_CAP` |

"Activity" is anything that updates the conversation: a message, a rename, a
pin. Opening a chat to read it is not activity.

The cap is now a backstop against runaway growth (a script creating chats, an
integration looping), not the everyday rule. At 500 a person has to start more
than five chats a day, every day, for the whole window before it applies.

Nothing about the sweep itself changed: the same hourly maintenance pass
(`docs/MAINTENANCE.md`), the same hard delete, the same cascade to the chat's
workspace files. A deployment that had set `CONVERSATION_TTL_DAYS` or
`CONVERSATION_UNPINNED_CAP` keeps its own values; only the defaults moved.

## What a person sees

Nothing, until a chat is inside its warning window. Then:

- **Its row in the sidebar** turns its hourglass to the warning colour and
  shows the days left (`5d`). The tooltip, and the row's description for a
  screen reader, say "will be deleted in 5 days" and list the ways to keep it.
  This is shown in both sidebar layouts; the plain hourglass on every
  temporary chat in the one-list layout is unchanged.
- **A line under the Recent / Temporary heading** counts the person's chats
  that are about to be deleted. It is a button: it scrolls to and focuses the
  first of them.
- **The chat itself** carries a notice above the transcript: how many days it
  has left, that sending a message keeps it for another full window, and a
  **Pin to keep** button.
- **Near the cap** (within a tenth of it), a second line under the heading
  says how many temporary chats the person has and what happens past the cap.

The ⓘ explainer now also names archiving as a way to keep a chat, which was
always true of the sweep and never said.

The numbers come from `GET /api/server-config` (`conversation_ttl_days`,
`conversation_expiry_warning_days`, `conversation_unpinned_cap`), and the
countdown is `updated_at` plus the window, computed in
`web/src/app/chat/ui/chatRetention.ts`. The warning window is held strictly
inside the inactivity window on both sides (`Config.ExpiryWarningDays`,
`retentionPolicy`): a warning as long as the window would flag every chat from
the moment it was created. A client that is not told the warning window shows
no countdown at all, because the countdown is a promise about what the server
will do.

## Scratch files have their own clock

Two file sweeps borrowed the conversation window as their age limit:
orchestrator upload staging (`temp_uploads`) and finished task and mcp-run
workspaces. Left alone, raising the chat window to 90 days would have kept six
times as much scheduler scratch on disk, which is the opposite of the goal.
They now read `FLEET_SCRATCH_RETENTION_DAYS` (default 14, their old effective
value).

Chat attachment uploads (`EMAIL_ATTACHMENT_DIR`) still follow the conversation
window, deliberately: an upload has to outlive the chat that refers to it.

## What this costs

Disk. Temporary chats, their workspace files and their uploads now stay for up
to 90 days instead of 14, so a box holds roughly the last three months of
throwaway work instead of the last two weeks. The bound is still a bound: the
window, the per-person cap, and the disk guard that sheds scheduled work below
`FLEET_DISK_MIN_FREE_PERCENT`. An operator who wants less sets
`CONVERSATION_TTL_DAYS` lower; the warning adapts.

## Deliberately not done

- **No email or push notification.** The warning is in the interface only, so
  someone who does not open Fleet during a chat's last 14 days is not told.
  An email was considered and left out: for an active person, chats age out
  every week, so the email would arrive every week about chats they had
  already abandoned.
- **No trash, no undo.** Deletion is still permanent
  (`FLEET_CONVERSATION_SOFT_DELETE` exists but has no restore endpoint or
  interface). A recoverable period would also hold the files for that much
  longer.
- **Reading is not activity.** A chat someone opens every week but never
  writes in still ages out; the in-chat notice is what catches that case.
- **No size-aware cleanup.** Every temporary chat gets the same window
  whether it holds two lines or two gigabytes of workspace files.
- **The cap is not warned per chat.** The sidebar says when a person is close
  to it, not which chats would go next.
