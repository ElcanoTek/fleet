# Scheduler UX 2.0 — upcoming runs + recurring context carry (#504)

Two forward-looking additions to the Operations Center scheduler, layered on the
existing task data with **no new run-records table**:

1. an **Upcoming runs** timeline — "what is fleet about to do?", and
2. **recurring context carry** — a recurring task's next run can start with a
   bounded summary of its previous run's output.

Both are opt-in-friendly and cheap: the timeline is a computed projection, and
context carry is deterministic (no extra LLM call, no whole-transcript replay).

## Upcoming runs

`GET /tasks/upcoming?limit=N` (default 50, admin/operator `view_tasks`
permission) projects each scheduled task's next executions and returns them
sorted soonest-first:

- **Recurring tasks** contribute up to their next 5 cron occurrences, computed
  with `cron.Next` **in the task's own timezone** (matching how the scheduler
  evaluates recurrence). An unparseable cron contributes nothing.
- **One-shot tasks** contribute their single future `scheduled_for`; a
  `scheduled_for` already in the past contributes nothing.

The feed is a *computed view* over `scheduled` + `pending` tasks — there is no
persisted run-records table, so the projection always reflects the current
schedule. Scoped principals only see runs for tasks within their scope.

Response shape:

```json
{
  "upcoming": [
    {
      "task_id": "…",
      "name": "daily-report",
      "prompt": "summarize yesterday's sales",
      "recurrence": "0 9 * * *",
      "next_run": "2026-07-02T09:00:00-04:00",
      "recurring": true
    }
  ]
}
```

The web **Operations Center → Upcoming** tab renders this grouped by calendar
day (Today / Tomorrow / weekday-date), each row showing the local time, the task
name (falling back to a prompt preview), and a schedule chip (a human cron
description for recurring, "One-time" for a one-shot).

## Recurring context carry

A task created with `carry_context: true` **and** a recurrence gets a bounded
handoff from its previous run injected into the next run's system prompt.

- **What is carried:** the previous run's **final assistant message only**,
  clamped to 2000 characters (a longer answer is truncated with a marker). No
  whole-transcript replay, no tool output, no separate summarization call — the
  handoff is read straight from the already-persisted last session log
  (`priorRunHandoff` in `internal/runner`).
- **How it reaches the model:** the runner installs the handoff on the run
  context via `scheduledrun.WithPriorRunContext`; `scheduledrun` renders it as a
  `## Previous Run` section (distinct from Captain's Log memory, learned
  instructions, and the resumed-after-ask Q&A), instructing the agent to
  continue from there rather than repeat completed work.
- **Scope:** recurring tasks only. `carry_context` on a one-shot task is inert
  (there is no "next run" to carry into). The first run of a recurring task has
  no prior log and so carries nothing.

`carry_context` is a nullable-defaulted boolean column (sched migration 050,
`DEFAULT FALSE`), threaded through the task model / storage / handlers / import
export exactly like `allow_network`, and toggled from the Task create modal's
Advanced settings.

### Why the final message, not a summary?

Carrying the last answer is deterministic, free, and predictable. A running
task's useful "state to carry forward" is almost always its conclusion (the
report it produced, the decision it reached), which is exactly the final
assistant message. Summarizing the whole transcript would add an LLM call, cost,
and a non-determinism the operator can't audit — for a feature whose value is
"pick up where you left off," the last answer is the honest primitive. See
[ADR-0025](adr/0025-scheduler-ux-context-carry.md).

## Honest scope / deferred

- **No run-records/run-history table.** The issue's "run history" is served today
  by the existing per-task log (`GET /logs/{task_id}`) + the live-run stream
  (#508); Upcoming adds the forward view. A durable per-occurrence history table
  (with per-run cost/duration rows and a calendar heatmap) is a larger,
  separate change.
- **Carry is last-message-only**, capped at 2000 chars — not a rolling
  multi-run memory (that is Captain's Log, #285) and not a summarizer.
- **No deep-linking** from an Upcoming row into a task editor — the web app has
  no per-task route; the row is informational. Opening a running task's live log
  is covered by the Recent Tasks table (#508).
