# ADR-0025: Recurring context carry is a bounded, deterministic last-message handoff

- **Status:** Accepted
- **Date:** 2026-07-02
- **Deciders:** fleet maintainers

## Context

Issue #504 (Scheduler UX 2.0) asks, among calendar/run-history UX, for a
recurring task to be able to "carry context" between runs so a daily/weekly job
can build on what its last run found instead of starting cold every time.

The obvious-but-wrong implementation is to replay the previous run's whole
transcript, or to summarize it with an LLM call at the start of each run. Both
add cost and — worse — non-determinism the operator cannot audit: the same
schedule would inject different context depending on a summarizer's mood, and a
long transcript would balloon the prompt (and the per-turn token ceiling) of
every subsequent run.

This ADR does not change any security invariant. It is recorded because it
establishes a run-to-run data-flow seam (`WithPriorRunContext`) and a deliberate
scope boundary, and because "carry context" is the kind of feature that invites
scope creep toward a second, weaker memory system.

## Decision

Recurring context carry is a **bounded, deterministic, last-message handoff**,
opt-in per task via a `carry_context` boolean.

1. **Carry the previous run's final assistant message only**, clamped to 2000
   characters. It is read straight from the already-persisted last session log
   (`priorRunHandoff`) — no new storage, no tool output, no extra LLM call. A
   running task's "state worth carrying" is its conclusion, which is the final
   answer.
2. **Inject via a context seam, rendered as one prompt section.** The runner
   installs the handoff with `scheduledrun.WithPriorRunContext`; `scheduledrun`
   renders a `## Previous Run` section, parallel to (and distinct from) the
   Captain's Log memory, learned-instruction, and resumed-after-ask sections.
   This reuses the established `WithWorkspaceReporter`/`WithArtifactCollector`
   /`WithAskHandler` seam pattern — run-scoped injection, no new governance path,
   the one `agentcore.Run` core untouched.
3. **Recurring-only, opt-in, off by default.** `carry_context` defaults FALSE
   (sched migration 050) and is inert on one-shot tasks. Off reproduces prior
   behaviour byte-for-byte (nil prior-run context → no section).

## Consequences

- **Deterministic and auditable:** the same schedule injects the same bounded
  text every run; an operator can read exactly what was carried in the run log.
- **Cheap:** zero added LLM calls, a hard 2000-char cap on the prompt cost, and
  no per-run history table to maintain.
- **Not a memory system.** Rolling, cross-many-run persistent memory is
  Captain's Log (#285); typed user/project memory is #515. Context carry is
  deliberately the minimal "continue from last time" primitive and is documented
  as such, so it is not mistaken for (or grown into) a competing store.
- **Deferred:** a transcript-summarizing carry mode and a durable
  per-occurrence run-history table are separate, larger changes (see
  [SCHEDULER-UX.md](../SCHEDULER-UX.md) honest scope).
