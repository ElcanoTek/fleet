# ADR-0035: Side-effect-gated recovery after stream commitment

Status: accepted (supersedes the suppression clause of ADR-0033)

## Context

ADR-0033 suppressed all in-run recovery — the in-place stream-blip retry that
predates it AND the fallback swap — after **any** semantic event (a single
text or reasoning delta) became observable in the failing attempt. A long
agentic round streams text virtually the whole time, so in practice every
transient mid-stream provider failure (e.g. an SSE 504 while the model
composed its final answer) aborted the run. The runner then classed the
untagged error as deterministic and dead-lettered the task as
`non_retryable` — with a configured `fallback_model` never consulted. A
14-minute scheduled protocol run was observed dying this way off one 504.

The invariant ADR-0033 actually protects is narrower than its gate: never
splice provider output into one completion, and never repeat a
potentially side-effecting tool sequence.

## Decision

Recovery after a transient mid-stream failure is gated on **tool side
effects, not on any semantic output** (`streamRoundWithResilience` +
`streamSink.toolEventCount`, enforced by
`TestStreamRoundRetriesBlipAfterTextOnlyOutput` and
`TestStreamRoundSuppressesRecoveryAfterToolExecution`):

- **Text/reasoning-only attempt** → recoverable. The attempt's partial
  accumulation is rolled back to the attempt-start mark
  (`streamSink.mark`/`rollbackTo`) and the trailing assistant message is
  dropped, then the round re-drives through the existing ladder (one in-place
  same-model retry, then the fallback chain). The regeneration **replaces**
  the abandoned partial, so nothing is spliced across providers and the
  persisted history / final answer carry no duplicate. Events already
  forwarded to a live Observer cannot be recalled; the live stream shows the
  abandoned partial followed by the retry note — a deliberate trade against
  killing the run.
- **Attempt that executed a tool** (a `tool_call`/`tool_result` became
  observable past the mark) → in-run recovery stays suppressed, exactly as
  ADR-0033 intends: a re-driven round restarts from its input messages, so
  the model could re-issue the executed calls and repeat their side effects.
  The error is tagged with the **transient** sentinel
  `ErrCommittedSideEffects`, so `runner.classifyFailure` routes it to the
  task's RetryPolicy (`FailureTransient`) — whole-task re-runs repeat side
  effects by definition, and `max_retries` is the operator's explicit opt-in
  to that — instead of mislabeling a provider timeout as a deterministic
  failure.

## Consequences

Mid-answer provider blips are survivable again for both interactive turns and
scheduled tasks; the fallback model regains its purpose for the most common
failure position (final-answer composition, where no tool is in flight).
Failures after a tool has run in the same attempt remain terminal for the
run, but are now honestly transient for the scheduler. The
`context-too-large → force-compact → re-drive` path keeps its pre-existing
behavior (it recovers a request rejected before streaming; no partial exists
to roll back).
