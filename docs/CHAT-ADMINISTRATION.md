# Chat administration

Operator tools for looking after chats that belong to someone else
([ADR-0115](adr/0115-chat-administration.md)). Operator usage is in
[`OPERATORS.md`](OPERATORS.md#chat-administration--fleet-chats-and-fleet-patrol).

## What shipped

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
- **Notices.** UI-only notes in the chat.
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
- **`fleet patrol pause|resume <id> --reason R`** goes through the owner's own
  path (a pause revokes a run in flight and cancels the patrol's queued runs),
  and is recorded and noted like every admin change.
- **Locking.** The edit locks the chat, then the schedule, the same order a
  fire's delivery uses, so an edit racing a firing patrol waits rather than
  deadlocking.

## Deviations

- The CLI does not reach the running server, so its message, stop and transfer
  wait for the next scheduler pass (30 s by default) instead of happening at
  once. `--wait` follows them to the end.
- The owner's own edit (`PATCH /conversations/{id}/schedules/{sid}`) does not
  rewrite runs already in the queue. The operator path does, because an
  operator edits without the owner's view of what is pending. Bringing the
  owner path in line is left for its own change.

### Deleting chats ([ADR-0116](adr/0116-admin-chat-delete-recycle-bin.md))

- **Delete → recycle bin.** The chat stops, its patrols are paused, it is
  tombstoned and its participants are revoked. It is restorable for 30 days,
  then purged.
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
- No bulk actions (the same message or patrol edit across many chats).
