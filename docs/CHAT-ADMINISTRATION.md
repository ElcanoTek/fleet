# Chat administration

Operator tools for looking after chats that belong to someone else. This note
covers the first piece, `fleet patrol` (a patrol's instructions changed safely
from the host). Operator usage is in
[`OPERATORS.md`](OPERATORS.md#patrol-set-prompt--changing-a-chat-patrols-instructions).

## What shipped

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
    instructions, so none of them runs the old ones.
  - **Notice.** It writes a UI-only `patrol_updated` notice into the chat,
    saying the instructions changed and why. The model never sees it.
- **Locking.** The edit locks the chat, then the schedule, the same order a
  fire's delivery uses, so an edit racing a firing patrol waits rather than
  deadlocking.

## Deviations

- The owner's own edit (`PATCH /conversations/{id}/schedules/{sid}`) does not
  rewrite runs already in the queue. The operator path does, because an
  operator edits without the owner's view of what is pending. Bringing the
  owner path in line is left for its own change.

## Deferred

- The broader chat administration suite (finding, reading, messaging and
  changing any chat, plus a browser page) builds on these store methods.
