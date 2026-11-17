# ADR-0112: Remove governed sub-agents

- **Status:** Accepted (supersedes ADR-0007; amends ADR-0085)
- **Date:** 2026-11-17
- **Deciders:** fleet maintainers

## Context

ADR-0007 added `spawn_subagent`: a native tool that let a governed run
delegate a scoped piece of work to a child `agentcore.Run`, with a sliced
budget, depth and fan-out caps, typed `explore`/`worker` roles, an isolated
working directory, child transcripts and live progress in both UIs. #1043
turned it on by default for scheduled tasks and interactive chat.

Nobody uses it. Runs that had the tool on their roster did their work
sequentially, and the feature is no longer the practice we recommend for
fleet's workloads. It was not free to keep: a 1,300-line spawn path, a second
finish policy (`NewDelegatedPolicy`), budget charge-back on both policies, a
progress relay through two stream formats, two transcript endpoints, a
per-task column, five settings, a system-prompt section on every run, and
matching cards, panels and translations in the web UI. Each of those was a
place another change had to keep "and a sub-agent inherits it" true.

## Decision

1. **Remove the feature outright.** `spawn_subagent`, the delegation prompt
   section, the child run path and its roles, child working directories,
   budget slicing and charge-back, the delegated finish policy, the
   `subagent.progress` event and `subagent_progress` task-stream frame, the
   child-transcript endpoints (`GET /logs/{task_id}/subagents/{child_session_id}`
   and `GET /conversations/{id}/subagents/{child_session_id}`), the
   `subagents_enabled` admin setting, the sub-agent counters in the admin
   server statistics, and every web surface for them are deleted. A run is
   one `agentcore.Run`; nothing spawns another. ADR-0001's single governed
   loop is unchanged, and now has one fewer adapter around it.
2. **Retire the settings.** `FLEET_SUBAGENTS_ENABLED`,
   `FLEET_SUBAGENTS_MAX_DEPTH`, `FLEET_SUBAGENTS_MAX_CHILDREN`,
   `FLEET_SUBAGENTS_BUDGET_FRACTION` and `FLEET_SUBAGENTS_MODEL` (and their
   `CHAT_`/`CUTLASS_` spellings) move to `envreg`'s `removedSettings`: a
   leftover line reads as unset and is reported by boot, `fleet doctor`,
   `fleet config explain` and `fleet validate-config --retired-only`. A
   persisted `subagents_enabled` workspace-settings row is never resolved.
3. **`allow_delegation` is accepted and ignored.** The field is gone from the
   task API, the task model and task exports. Inputs that still carry it keep
   working: the JSON and YAML task decoders are not strict, so an old export,
   an API client or an intake app that sends the key is unaffected; the
   bundle manifest decoder *is* strict, so the task-template struct keeps the
   key as a no-op that is not served to the web.
4. **The `tasks.allow_delegation` column stays** as inert schema, per
   docs/MIGRATIONS.md (stop reading a column before any migration drops it)
   and ADR-0111's precedent. It is `NOT NULL` with a default, so inserts omit
   it. `retiredTaskColumns` (`internal/sched/db/task_columns.go`) names it so
   the schema↔registry drift gate still fails on any *other* unregistered
   column.
5. **History is shown best-effort.** A stored conversation or run log that
   contains a `spawn_subagent` call or a `subagent_spawned` entry renders
   through the generic tool chip / log entry. Child session-log files already
   on disk are left where they are; nothing serves them.

## Consequences

- **The `/v1` contract shrinks without a major bump.** docs/api-versioning.md
  classifies removing an endpoint or a response field as breaking. The
  transcript endpoint and the task `allow_delegation` field leave `/v1`
  anyway, as ADR-0111's endpoints did: the API has no GA date, the feature had
  no users, and a `/v2` or a permanent shim for an endpoint nobody calls would
  keep exactly the code this ADR removes. The exception is recorded in
  docs/api-versioning.md ("Not yet implemented").
- The model is never offered `spawn_subagent`, and no system prompt mentions
  delegation. Work that used to fan out runs in the one loop; a task that
  genuinely needs separate runs uses `create_task` or A2A (ADR-0056), both of
  which are separate governed runs with their own budgets.
- ADR-0085's rule that a chat turn's hidden tools are inherited by its
  sub-agents has nothing left to apply to; the hiding itself is unchanged.
- Context elision (docs/SCHEDULED-COMPACTION-CHECKPOINTS.md) now keeps only
  the task-tracker plan; a historical `spawn_subagent` result in a replayed
  conversation is elided like any other tool result.
- The end-of-run verifier and the self-audit finish gate apply to every
  scheduled run, with no exemption.
- A follow-on migration may drop `tasks.allow_delegation` once no supported
  binary predates this change.
