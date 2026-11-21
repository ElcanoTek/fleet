# Chat administration

Operator tools for looking after chats that belong to someone else
([ADR-0115](adr/0115-chat-administration.md); who needs help, the read-only
view and pinned notes are [ADR-0117](adr/0117-helping-a-chats-people.md)). Operator usage is in
[`OPERATORS.md`](OPERATORS.md#chat-administration--fleet-chats-and-fleet-patrol).

## What shipped

### Helping a chat's people ([ADR-0117](adr/0117-helping-a-chats-people.md))

- **Who needs help.** Settings → Admin → Chats opens on four counts (need
  attention, running now, with patrols, pinned notes) that filter the page,
  and on the chats grouped by the person who owns them, people with chats
  needing attention first. A flat list, archived chats and the recycle bin are
  one click away. `fleet chats people` and `fleet chats list --attention` /
  `--state` are the same from a terminal.
- **Needs attention** means exactly: an approval card is waiting in the chat,
  or its latest turn ended in an error.
- **One chat, as its owner sees it.** A chat opens on its own
  (`?chat=<id>`): what it is waiting on (each waiting card's tool, for whom,
  how long, when it is denied unanswered), then the conversation rendered as
  the chat renders it (bubbles, formatted replies, patrol runs, administrator
  messages, notices), read-only, with tool calls on request and earlier
  messages paged in. Files and images are named, not fetched. It refreshes
  every 8 seconds while visible. Tabs hold the patrols, the settings (title,
  model, approvals, hand over, archive, delete) and the record of admin
  changes.
- **Step in.** One panel under the conversation, saying for each choice who
  sees it and whether the assistant reads it: pin a note, message the
  assistant, add a notice.
- **Pinned note.** One per chat, at most 2000 characters, set with the page or
  `fleet chats note`. The chat's owner and Multiplayer participants see it
  above the conversation as *Note from an administrator · email*. It is stored
  in `chat.admin_notes`, outside the chat's history, so the model never reads
  it. *Got it* records the first person who acknowledged it and folds the
  banner to one line; replacing the text makes it unread again. An open chat
  picks up a new, replaced or removed note within about a minute. In the
  browser, replacing a note is a compare-and-swap on the version the edit
  started from, so two administrators cannot silently overwrite each other
  (409, and the draft is kept). Recorded as
  `note_set` / `note_replace` / `note_clear`. It takes no reason and writes no
  notice: the note itself is what the chat's people are shown.
- **Phones.** The page and the banner are laid out for a 390px screen; the
  browser spec (`web/e2e/mocked/admin-chats.spec.ts`) runs at desktop and phone
  widths and fails on sideways scrolling or an axe violation.

### Chats: `fleet chats` and Settings → Admin → Chats

- **Find and read.** Chats are listed across every user, filtered by owner,
  title or id, and activity. Each shows its owner, model, approval mode,
  patrols, waiting cards, queue and running turn. The transcript tail can
  include tool calls.
- **Message, stop, transfer.** These are recorded in `chat.admin_requests` and
  carried out by the running server: on the scheduler pass for the CLI, at once
  for the browser.
  - A message becomes a queue row of mode `admin`. It runs as the owner, in
    Manual approvals whatever the chat's mode, with the administrator
    recorded on the entry (`admin_email`), kind `admin_message`. The owner
    sees the turn's full work, as for their own turns.
  - Stop is the owner's Stop. Transfer is the owner's transfer: one function,
    `transferConversation`, which both paths call.
- **Settings.** Title (manual rename path), model (not on a Lockdown chat) and
  approval mode (down to manual only), validated together before any is
  written. Archive is a server request, because it revokes a shared chat's
  participants.
- **Notices.** UI-only lines in the conversation.
- **Accountability.** Every change takes a reason, is recorded in
  `chat.admin_actions` with its actor and surface, and is announced in the
  chat.
- **In the chat.** An admin message renders as *Message from an administrator ·
  email*, never as the owner's bubble. The chat guide (en/es/pt) explains it.

### Patrols: `fleet patrol` (and the patrol panels on the Chats page)

- **`fleet patrol list`** shows every user's patrols (and timers with
  `--timers`), with the sha256 of each prompt. **`fleet patrol show <id>`**
  shows one; `--prompt-only` prints the prompt byte for byte for editing.
- **`fleet patrol set-prompt <id> --file F --expect-sha256 S --reason R`**:
  - **Compare-and-swap.** It refuses an edit built from text that has changed
    since (the sha256 is checked before the diff and again under the lock).
  - **Diff first.** It prints the diff, and `--dry-run` stops there.
    Applying without a terminal needs `--yes`.
  - **Backup.** The old prompt is saved to
    `/var/backups/fleet/patrol-prompts/` (mode 0600) before the change, and the
    restore command is printed.
  - **Prompt and title only.** Owner, status, cron and next run are untouched.
  - **Scheduler bookkeeping.** It bumps `def_version` exactly as the owner's
    edit does, so a fire claimed but not yet delivered is superseded.
  - **Queued runs are rewritten.** Runs already delivered into the chat's
    input queue (waiting behind a running turn) are rewritten to carry the new
    instructions, so none of them runs the old ones. A run already running
    finishes with the instructions it started with; the command says so.
  - **Deleted chats.** A soft-deleted chat's patrols are neither listed nor
    editable.
  - **Notice.** It writes a UI-only `patrol_updated` notice into the chat,
    saying the instructions changed and why. The model never sees it.
- **`fleet patrol set-schedule <id> [--cron C] [--timezone TZ] --expect-def-version N --reason R`**
  (and **Change schedule** on the Chats page, `PATCH /admin/patrols/{id}` with
  `cron` / `timezone` / `expect_def_version`):
  - **Compare-and-swap on `def_version`.** Any edit of the patrol's definition
    since it was read (its prompt, title or schedule, by the owner or another
    administrator) bumps `def_version`, and the edit is refused. `fleet patrol
    show` prints the value.
  - **The owner's own edit path.** The new schedule goes through the same code
    as the owner's `PATCH /conversations/{id}/schedules/{sid}`
    (`updateLockedChatScheduleTx`): the same cron parser, timezone check and
    5-minute minimum interval, the same `def_version` bump (a fire claimed
    under the old schedule is superseded) and the same next-run computation.
  - **Preview first.** A dry run (`--dry-run`, or `dry_run: true`) runs every
    check and prints the next five runs in the patrol's timezone without
    changing anything. The browser previews this way as you type, so what it
    shows is what the scheduler will do. Applying without a terminal needs
    `--yes`.
  - **Schedule only.** Owner, prompt, title and status are untouched. A paused
    patrol stays paused; its new schedule is validated now and its next run is
    computed when it is resumed.
  - **Patrols only.** A one-shot timer has no schedule to change, and a
    cancelled or completed patrol is refused.
  - **Record and notice.** It is recorded as `patrol_schedule` (old and new
    cron and timezone, and the `def_version` change) and announced in the chat
    with a UI-only `patrol_updated` notice: "An administrator changed the
    schedule of the patrol … from … to …. Reason: …". Applying the schedule a
    patrol already has changes nothing and writes neither.
  - A run already delivered into the chat's queue is a run of the old schedule
    that has happened; it is left as it is, as on the owner's path.
- **`fleet patrol pause|resume <id> --reason R`** goes through the owner's own
  path (a pause revokes a run in flight and cancels the patrol's queued runs),
  and is recorded and noted like every admin change.
- **Locking.** The prompt and schedule edits lock the chat, then the schedule,
  the same order a fire's delivery uses, so an edit racing a firing patrol
  waits rather than deadlocking.

## Deviations

- The CLI does not reach the running server, so its message, stop and transfer
  wait for the next scheduler pass (30 s by default) instead of happening at
  once. `--wait` follows them to the end.
- The owner's own edit (`PATCH /conversations/{id}/schedules/{sid}`) does not
  rewrite runs already in the queue. The operator path does, because an
  operator edits without the owner's view of what is pending. Bringing the
  owner path in line is left for its own change.
- Sharing the owner's edit path changed it in one respect: a new cron or
  timezone on a **paused** patrol is now validated when it is set. Before, it
  was stored unchecked and refused only when someone resumed the patrol.

### Deleting chats ([ADR-0116](adr/0116-admin-chat-delete-recycle-bin.md))

- **Delete → recycle bin.** The chat stops, every patrol and timer in it is
  paused and it is tombstoned, in one transaction. Sharing, participants and
  their paused patrols are left as they were, so a restore gives the chat
  back whole. It is restorable for 30 days, then purged.
- **Restore.** The chat comes back, with its patrols still paused.
- **Delete permanently.** For emergencies: the chat is gone now and the
  workspace sweep reclaims its files. You type the chat's title (browser) or
  pass `--permanent --yes` (CLI).

## Deferred

- Answering someone else's approval cards and turning on Autonomous mode stay
  with the owner on purpose (ADR-0115).
- No bulk delete; the storage clean-up on Settings → Admin → Server covers
  old unused chats in bulk.
- An open chat tab does not refresh live when an administrator posts a notice
  or changes a setting; the change shows on the next load or turn.
- No bulk actions (the same message, note or patrol edit across many chats).
- "Seen" on a pinned note is one mark per note, not one per person: in a
  Multiplayer chat it says someone read it, not that everyone did.
- A chat whose latest turn failed stays in "needs attention" until its next
  turn, or until that turn's record is swept (`FLEET_TURN_EVENT_RETENTION_DAYS`,
  14 days by default); there is no dismiss.
- Not built on purpose: an administrator opening someone's chat in the chat
  view and typing as them (ADR-0117).
- The administrator's view of a conversation does not draw approval cards or
  generated-UI cards as the chat does; it lists the waiting cards above the
  conversation instead: the 50 that have waited longest, with the number
  still waiting beyond them when there are more.
