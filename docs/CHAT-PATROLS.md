# In-Chat Patrols & Timers (`chat_patrol`)

**Status:** shipped 2026-10-28.

## Overview

Traders and operators often want their assistant to stay on patrol inside an active chat conversation — checking deal pacing, margin anomalies, backfills, or refreshing Pages dashboards every few hours (or waking up after a one-shot delay) with the conversation's full transcript, workspace files, Python variables, and enabled MCP connectors preserved.

Previously, recurring work required promoting a conversation to a standalone headless job in the Operations Center (`schedule_task`), where each occurrence starts in a fresh transcript outside the chat thread. In-chat patrols (`chat_patrol`) complement Operations Center tasks by scheduling recurring cron turns or one-shot timers directly on a `conversations` row.

## Shipped Architecture

1. **Persistence (`086_chat_schedules.sql`, `087_input_queue_patrol_mode.sql`, `088_input_queue_patrol_mode_validate.sql`, `internal/store/chat_schedules.go`)**
   - `chat_schedules` stores per-conversation recurring cron patrols (`cron_expr` with `MinChatPatrolCronInterval = 5m`, validated across 48 consecutive firings) and one-shot delay timers (`duration_seconds` between 10s and 30d), up to `MaxActiveSchedulesPerConversation = 5`.
   - Creating a recurring cron patrol on a conversation that already has an active or paused recurring patrol updates the existing patrol in place by default (`AllowMultiple = false`), preventing accidental duplicate patrols when adjusting cadence or instructions.
   - `SweepExpired` exempts conversations that hold an active or paused schedule in `chat_schedules` so long-running patrol threads are never reaped while active.
   - `ClaimDueChatSchedules` and `TriggerChatScheduleNow` atomically advance `run_count` / `next_run_at` / `status` and enqueue a `chat_input_queue` row with server-owned `mode = 'patrol'` (`store.InputModePatrol`). If the conversation already has a queued or running patrol turn, a pending non-preview approval card, or a full input queue, due schedules coalesce and advance `next_run_at` (recurring patrols to the next cron tick; one-shot timers by 30s) so blocked schedules never pile up or starve later conversations.

2. **Turn Execution, Approvals & Python State (`internal/httpapi/`, `internal/tools/`)**
   - Draining a `mode = 'patrol'` queue row launches a governed chat turn with `inputKind = agent.InputKindChatPatrol` (`"chat_patrol"`), rendered in the transcript as a collapsible `PatrolTurnNotice` card rather than a user bubble.
   - The interactive `chat_patrol` tool (`create`, `list`, `update`, `pause`, `resume`, `cancel`) lets the assistant manage the current conversation's schedule in-turn and emits `patrol.updated` SSE events so the UI updates live.
   - Model-callable `chat_patrol` never elevates `conv.AutoApprove` without explicit user consent; only user-authenticated HTTP requests (`POST /api/conversations/{id}/schedules` or `PATCH /api/conversations/{id}`) can enable `conv.AutoApprove`.
   - When a patrol turn stages a critical tool approval card while `conv.AutoApprove` is off, the card receives at least a 24-hour (`86400s`) expiry floor and automatically arms `resume_after_approval`, so approving the card later immediately resumes the patrol turn.
   - `python_bridge.py` automatically checkpoints and restores serializable user variables (`.fleet_python_state.pkl` in the conversation workspace) across turns and patrol runs, and persistent sandboxes scope their writable bind-mount to `<WorkspaceHostDir>/<convID>` and reap any detached non-kernel processes on turn release.

3. **Web UI (`PatrolBanner.tsx`, `Composer.tsx`, `chat-experience.tsx`)**
   - `GET /api/conversations/{id}` and `/api/conversations/{id}/schedules` hydrate the conversation's active/paused schedules.
   - The composer includes a **Patrol** popover with 1-click presets (`Every 1h`, `Every 2h`, `Every 4h`, `Every 6h`, `In 30m (1x)`, `Custom`), instructions input, and an optional `Auto-approve tools while patrolling` toggle.
   - The `PatrolBanner` header bar displays live countdowns, run counts, and 1-click **Run now**, **Pause/Resume**, **Stop**, and **⚡ Enable Auto-approve** controls, and polls while a patrol is active so background patrol turns stream into open tabs automatically.

## Deferred Scope

- Cross-conversation patrol dashboards (listing all active chat patrols across every conversation on one admin screen) are deferred; Operations Center remains the board for fleet-wide scheduled jobs.
