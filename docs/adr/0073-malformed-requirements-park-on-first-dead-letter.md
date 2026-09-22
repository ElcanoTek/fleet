# ADR-0073: A malformed EXECUTION REQUIREMENTS declaration parks its chain on the first dead-letter

- **Status:** Accepted
- **Date:** 2026-09-23
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0070](0070-dead-lettered-recurrences-spawn-successor.md). A
  dead-lettered occurrence whose prompt carries a malformed declaration parks
  on the first strike, not the second.

## Context

ADR-0070 lets a dead-lettered recurring occurrence spawn its successor, and
parks the chain when two consecutive occurrences dead-letter: "one bad day
continues, two is systemic". That threshold assumes a cause that might not
recur.

A malformed `EXECUTION REQUIREMENTS (JSON)` line is not such a cause (#1601).
It is a property of the prompt text, which every successor copies verbatim,
and the dispatch preflight refuses it before any model or tool work. Prod
2026-09-21: `f76c1aa0` (raptive-depop) and `3591c573` (husqvarna)
dead-lettered at $0 on `"fast_io + fastio_helpers"` in `mcp_servers`. The live
successor `1c002f25` carried the same line, so it was certain to dead-letter
again the next weekday and then park — two notifications, a lost day, and
nothing that could have gone differently in between.

## Decision

1. **Refuse the declaration at save time.** Every task write path runs
   `models.ValidateExecutionRequirements`: create, edit, clone and rerun, HTTP
   import, batch, estimate, and the CLI imports. The rejection names the
   offending identifier and the allowed pattern. The dispatch parser calls the
   same function first, so dispatch and save-time validation cannot disagree.
2. **Park on the first dead-letter.** In `storage.scheduleNextRecurrence`, a
   dead-lettered occurrence whose prompt fails that validation parks at once:
   the same `recurrence_spawned = TRUE` + `recurrence_parked_at` settlement as
   the two-strike breaker, and no successor. The log line names the
   validation error and says to correct the prompt. Replay reruns the same
   prompt and cannot help; the task editor saves a corrected copy.

Every other dead-letter keeps ADR-0070's two-strike rule unchanged, including
an execution-requirements failure that is **not** malformed (an unavailable
tool or server, network egress off): those can change without editing the
prompt.

## Enforcement

- `internal/sched/models/execution_requirements.go`:
  `ValidateExecutionRequirements`, the one grammar.
- Call sites:
  - `internal/scheduledrun/requirements.go`: `parseExecutionRequirements`
    calls it first;
  - `internal/sched/handlers/handlers.go`: `validateTaskCreate`;
  - `internal/admincli`: `validateImportedTask`, `validateExportRecordCLI`,
    `validateBatchTaskCreate` and `buildImportedTask`.
- `internal/sched/storage/storage.go`: `scheduleNextRecurrence`, where the
  malformed check precedes the predecessor lookup.
- Tests:
  - `TestDeadLetterBreakerParksMalformedRequirementsOnFirstDeadLetter`
    (storage);
  - `TestParseExecutionRequirementsAgreesWithSaveTimeValidation`
    (scheduledrun);
  - `TestTaskWritesRejectMalformedExecutionRequirements` and
    `TestValidateTaskCreate_ExecutionRequirements` (handlers);
  - `TestCLIWritePathsRejectMalformedExecutionRequirements` (admincli);
  - `TestValidateExecutionRequirements` (models).

## Consequences

- A malformed line can no longer be saved. A task saved before this shipped
  (like `1c002f25`) dead-letters once, with the offending identifier in its
  reason, and parks. It sends one notification instead of two.
- Recovery changes: replay alone does not continue such a chain, because the
  definition is what is broken. The owner corrects the prompt in the task
  editor, which saves a corrected copy.

## Alternatives considered

- **Don't count a malformed dead-letter toward the breaker.** The chain would
  keep spawning $0 dead-letters every period until someone edits a scheduled
  successor: daily notifications for a failure that is certain to repeat, and
  indefinitely for a forgotten task.
- **Refuse to spawn the successor without parking.** This would end the chain
  silently, with no replay path.
