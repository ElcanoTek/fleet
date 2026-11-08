# In-Chat Patrols & Timers (`chat_patrol`)

**Status:** shipped 2026-10-28.

## Overview

Traders and operators often want their assistant to stay on patrol inside an active chat conversation — checking deal pacing, margin anomalies, backfills, or refreshing Pages dashboards every few hours (or waking up after a one-shot delay) with the conversation's full transcript, workspace files, Python variables, and enabled MCP connectors preserved.

Previously, recurring work required promoting a conversation to a standalone headless job in the Operations Center (`schedule_task`), where each occurrence starts in a fresh transcript outside the chat thread. In-chat patrols (`chat_patrol`) complement Operations Center tasks by scheduling recurring cron turns or one-shot timers directly on a `conversations` row.

## Shipped Architecture

1. **Persistence and firing (`sched` migrations `076_chat_schedules` and `077_move_chat_schedules`, `087_input_queue_patrol_mode.sql`, `088_input_queue_patrol_mode_validate.sql`, `internal/store/chat_schedules.go`, `internal/sched/db/chat_schedules.go`, `internal/sched/scheduler/chat_schedules.go`)**
   - Since #2049 M5 ([ADR-0104](adr/0104-one-scheduler.md)) the rows live in the scheduler's schema (`sched.chat_schedules`, moved from `chat.chat_schedules` by sched migration 077) and are fired by the scheduler's one tick loop, not a chat-side sweeper.
   - `chat_schedules` stores per-conversation recurring cron patrols (`cron_expr` with `MinChatPatrolCronInterval = 5m`, validated across 48 consecutive firings) and one-shot delay timers (`duration_seconds` between 10s and 30d), up to `MaxActiveSchedulesPerConversation = 5`.
   - Creating a recurring cron patrol on a conversation that already has an active or paused recurring patrol updates the existing patrol in place by default (`AllowMultiple = false`), preventing accidental duplicate patrols when adjusting cadence or instructions.
   - `SweepExpired` exempts conversations that hold an active or paused schedule in `chat_schedules` so long-running patrol threads are never reaped while active.
   - Each fire is claim → delivery → settle. The scheduler leases a due row (`lease_owner` / `lease_expires_at`, the tasks' lease model); the chat server's `DeliverChatScheduleFire` enqueues a `chat_input_queue` row with server-owned `mode = 'patrol'` (`store.InputModePatrol`) under the idempotency key `chat-patrol:<id>:<run>:<s|m>-<fire_id>` (s scheduled, m Run now); the settle advances `run_count` / `next_run_at` / `status`, releases the lease and, for a fire that ran (`queued`) or was turned away (`busy`, `gone`), appends a row to `sched.chat_schedule_runs` (the run history, pruned with task runs by `FLEET_RUN_LOG_RETENTION_DAYS`); a Run now that coalesces onto a turn in flight (409) or is refused on a full queue (429) adds none. A fire that dies before it settles (or whose delivery failed, after a backoff) is re-claimed as itself by the recovery pass when its lease expires, whatever the schedule's status, and its re-delivery finds its own queue row. Pausing or cancelling a schedule revokes a fire in flight. Deleting a conversation deletes its schedules (a trigger on `chat.conversations` that sched migration 077 creates — or `fleet serve` at startup, on a database the scheduler initialised first — and 077's down removes). **Run now** takes the same path through `Scheduler.RunChatScheduleNow`. If the conversation already has a queued or running patrol turn, a pending non-preview approval card, or a full input queue, due schedules coalesce and advance `next_run_at` (recurring patrols to the next cron tick; one-shot timers by 30s) so blocked schedules never pile up or starve later conversations.

2. **Turn Execution, Approvals & Python State (`internal/httpapi/`, `internal/tools/`)**
   - Draining a `mode = 'patrol'` queue row launches a governed chat turn with `inputKind = agent.InputKindChatPatrol` (`"chat_patrol"`), rendered in the transcript as a collapsible `PatrolTurnNotice` card rather than a user bubble.
   - The interactive `chat_patrol` tool (`create`, `list`, `update`, `pause`, `resume`, `cancel`) lets the assistant manage the current conversation's schedule in-turn and emits `patrol.updated` SSE events so the UI updates live.
   - Model-callable `chat_patrol` never elevates `conv.AutoApprove` without explicit user consent; only user-authenticated HTTP requests (`POST /api/conversations/{id}/schedules` or `PATCH /api/conversations/{id}`) can enable `conv.AutoApprove`.
   - When a patrol turn stages a critical tool approval card while `conv.AutoApprove` is off, the card receives at least a 24-hour (`86400s`) expiry floor and automatically arms `resume_after_approval`, so approving the card later immediately resumes the patrol turn.
   - `python_bridge.py` automatically checkpoints and restores serializable user variables (`.fleet_python_state.pkl` in the conversation workspace) across turns and patrol runs, and persistent sandboxes scope their writable bind-mount to `<WorkspaceHostDir>/<convID>` and reap any detached non-kernel processes on turn release.

3. **Web UI (`PatrolBanner.tsx`, `PatrolSchedulePicker.tsx`, `patrolSchedule.ts`, `Composer.tsx`, `chat-experience.tsx`)**
   - `GET /api/conversations/{id}` and `/api/conversations/{id}/schedules` hydrate the conversation's active/paused schedules.
   - The composer includes a **Patrol** popover with 1-click presets (`Every 1h`, `Every 2h`, `Every 4h`, `Every 6h`, `Weekdays 9am`, `Daily 9am`, `In 30m (once)`, `In 2h (once)`, `Custom`), instructions input, and an optional `Auto-approve tools while patrolling` toggle. Selecting `Custom` opens a visual schedule/timer builder supporting recurring schedules (`Every N hours`, `Every N minutes`, `Every day`, `Weekdays (Mon–Fri)`, `Specific days of the week` with Mon–Sun toggles, time picker, IANA time-zone dropdown with `Custom zone` entry, live `describeCronExpression` preview, and an `Advanced cron` backup input) as well as custom one-shot timers (`Minutes` / `Hours` up to 1440m / 168h).
   - The `PatrolBanner` header bar displays human-readable cadences (`formatScheduleCadence`), live countdowns, run counts, and 1-click **Run now**, **Pause/Resume**, **Stop**, and **⚡ Enable Auto-approve** controls, and polls while a patrol is active so background patrol turns stream into open tabs automatically.

## Deferred Scope

- Cross-conversation patrol dashboards (listing all active chat patrols across every conversation on one admin screen) are deferred; Operations Center remains the board for fleet-wide scheduled jobs.
