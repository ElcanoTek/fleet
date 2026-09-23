# ADR-0073: A malformed EXECUTION REQUIREMENTS declaration parks its chain on the first dead-letter

- **Status:** Accepted
- **Date:** 2026-09-23
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0070](0070-dead-lettered-recurrences-spawn-successor.md). A
  dead-lettered occurrence whose prompt carries a malformed declaration parks
  on the first strike, not the second.
- **Revised:** 2026-09-23, in the #1611 follow-up. The park now records its
  reason and the notification carries it. A plain replay of a malformed prompt
  is refused, and replay takes a corrected prompt. The recovery text and the
  enforcement list were corrected.

## Context

ADR-0070 lets a dead-lettered recurring occurrence spawn its successor, and
parks the chain when two consecutive occurrences dead-letter: "one bad day
continues, two is systemic". That threshold assumes a cause that might not
recur.

A malformed `EXECUTION REQUIREMENTS (JSON)` line is not such a cause (#1601).
It is a property of the prompt text, which every successor copies verbatim,
and the dispatch preflight refuses it before any model or tool work. In
production, two recurring page refreshes dead-lettered at $0 on the same day
on `"fast_io + fastio_helpers"` in `mcp_servers`. A live successor carried the
same line, so it was certain to dead-letter
again the next weekday and then park — two notifications, a lost day, and
nothing that could have gone differently in between.

## Decision

1. **Refuse the declaration at save time.** Every task write path checks the
   prompt with `models.ValidateExecutionRequirements`:
   - create, edit, clone and rerun, HTTP import, batch and estimate;
   - the CLI imports and batch;
   - the chat `schedule_task` / `manage_tasks` calls, both before their
     approval card is staged and again at the storage seam after approval.

   The rejection names the field, index and offending identifier, and the
   allowed pattern. There is one parser, `models.ParseExecutionRequirements`,
   and dispatch reads the declaration through it, so dispatch and save-time
   validation cannot disagree.
2. **Park on the first dead-letter, and say so.** In
   `storage.scheduleNextRecurrence`, a dead-lettered occurrence whose prompt
   fails that validation parks at once, with no successor. It gets the same
   `recurrence_spawned = TRUE` + `recurrence_parked_at` settlement as the
   two-strike breaker, plus `recurrence_parked_reason` (migration 073), the
   validation error in words the owner can act on.
   - The dead-letter write returns the reason to the runner. The failure
     notification then carries "Schedule stopped: <reason>" in its message
     (email, or a webhook template that uses `.Message`). This holds when the
     park happens in the dead-letter write, the usual path. If a transient
     database error defers the park to the `ReconcileRecurrences` sweep, the
     sweep records the same reason and the Operations Center shows it, but no
     second notification is sent: the failure notification already went out
     without the stop line.
   - The Operations Center shows the occurrence's schedule as stopped, with the
     reason. The two-strike park records its own reason the same way.
3. **Recover by replaying with a corrected prompt.** A plain replay would rerun
   the malformed prompt and dead-letter again, so
   `ReplayDeadLetteredTaskWithPrompt` refuses it with the validator's message,
   and `fleet sched dlq replay` exits 1 (a refused write).
   - `fleet sched dlq replay --prompt-file <file> <id>` replaces the prompt
     (validated) and replays the same row. The schedule, the task memory and
     the lineage continue, and the successor carries the corrected prompt.
   - The alternatives lose the chain's task memory, which only the recurrence
     spawn carries forward:
     - recreate the task with the corrected prompt and its recurrence;
     - clone it with the prompt overridden (`POST /tasks/{id}/clone` with
       `{"overrides":{"prompt":"…"}}`; a clone without the override copies
       the malformed prompt and is refused).
   - The web editor's save on a dead-lettered task is a one-off rerun that
     drops the recurrence.

Every other dead-letter keeps ADR-0070's two-strike rule unchanged, including
an execution-requirements failure that is **not** malformed (an unavailable
tool or server, network egress off): those can change without editing the
prompt.

## Enforcement

- `internal/sched/models/execution_requirements.go`:
  `ParseExecutionRequirements`, the one parser. `ValidateExecutionRequirements`
  is its result-free form.
- Call sites:
  - `internal/scheduledrun/requirements.go`: `parseExecutionRequirements`, a
    thin adapter embedding `models.ExecutionRequirements`;
  - `internal/sched/handlers`: `validateTaskCreate` (handlers.go) and
    `validateExportRecord` (task_export_import.go);
  - `internal/sched/storage/storage.go`: `EnqueueTaskAs` and
    `UpdateEditableTask`, the seams the approved chat tools reach;
  - `internal/httpapi/approvals.go`: `prevalidateStagedTaskPrompt`, before
    `Stage` creates a card;
  - `internal/admincli`: `validateImportedTask`, `validateExportRecordCLI`,
    `validateBatchTaskCreate` and `buildImportedTask`.
  - Not validated: `AddTaskWithContext`, `AddTaskBatch` and
    `ReplaceTaskDefinition`. Every caller validates first, except the
    webhook/email trigger run, whose prompt is rendered from the event and can
    still fail at dispatch.
- `internal/sched/storage/storage.go`:
  - `scheduleNextRecurrence` / `deadLetterParkReason`: the malformed check
    precedes the predecessor lookup, and the reason is written with the park
    stamp;
  - `ReplayDeadLetteredTaskWithPrompt`: the replay guard and the corrected
    prompt.
- `internal/runner/notify.go`: `scheduleStoppedMessage`.
- `web/src/app/orchestrator/taskDisplay.tsx`: `scheduleStoppedReason`.
- Tests:
  - storage:
    - `TestDeadLetterBreakerParksMalformedRequirementsOnFirstDeadLetter`;
    - `TestMalformedParkRecordsWhyAndResumesWithACorrectedPrompt`;
    - `TestTwoStrikeParkRecordsWhy`;
  - `TestDeadLetterNotificationSaysTheScheduleStopped` (runner);
  - `TestParseExecutionRequirementsIsTheModelsParser` (scheduledrun);
  - handlers:
    - `TestTaskWritesRejectMalformedExecutionRequirements`;
    - `TestValidateTaskCreate_ExecutionRequirements`;
  - `TestStageRefusesAMalformedTaskPromptBeforeStaging` (httpapi);
  - admincli:
    - `TestCLIWritePathsRejectMalformedExecutionRequirements`;
    - `TestSchedDLQReplayRefusesMalformedAndTakesAPromptFile_DB`;
  - models:
    - `TestValidateExecutionRequirements`;
    - `TestParseExecutionRequirements`;
  - web: `LogViewer.test.tsx` and `taskDisplay.test.ts`.

## Consequences

- A malformed line can no longer be saved. A task saved before this shipped
  (like that live successor) dead-letters once, with the offending identifier
  in its reason, and parks. It sends one notification instead of two, and that
  notification says the schedule stopped and why (unless the park is deferred
  to the reconciliation sweep, above).
- **Recovery changes.** Replay alone does not continue such a chain, because
  the definition is what is broken, and it is refused. A replay with the
  corrected prompt continues the same chain with its task memory. Recreating
  or cloning with a corrected prompt starts a new chain without it. An edit
  from the web editor alone would start one run and leave the schedule dead.
- **Operator-only recovery.** The correct-and-resume path is CLI-only, like
  replay itself. There is no web control for it yet.

## Alternatives considered

- **Don't count a malformed dead-letter toward the breaker.** The chain would
  keep spawning $0 dead-letters every period until someone edits a scheduled
  successor: daily notifications for a failure that is certain to repeat, and
  indefinitely for a forgotten task.
- **Refuse to spawn the successor without parking.** This would end the chain
  silently, with no replay path.
