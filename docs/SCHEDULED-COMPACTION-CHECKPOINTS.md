# Scheduled runs: compaction checkpoints inside the tool loop

**Status:** shipped 2026-09-21. Design note for the change that makes the
cost-aware compaction (#1534) recur inside a scheduled run's tool loop and gives
scheduled runs a real compaction summary.

## The problem, as observed

On 2026-09-21 every long scheduled run on fleet prod hit the 10M uncached-token
ceiling or came close: 9–15M prompt tokens per run, $5–8 each, seven
dead-letters in one day. The transcripts showed why:

- A scheduled run is **one fantasy round** of 100–130 tool steps until the
  completion policy rejects the finish. `checkContextPressure` — where the
  resend-budget compaction lives — runs *before a round*, so it fired at most
  once per verifier round. The latest run compacted once at step 55 (137K
  resent, 52 turns removed) and then ran 78 more steps at ~150K tokens each.
- The scheduled driver wired **no `CompactionSummarizer`**, so that one
  compaction replaced 52 turns with a one-line placeholder. The run then
  re-derived what it had already established.

## What shipped

1. **Resend-budget checkpoint** (`internal/agentcore/engine.go`,
   `resendBudgetCheckpoint`): for a scheduled engine
   (`RequireCompactionOptIn`) with `FLEET_CONTEXT_RESEND_BUDGET_TOKENS > 0`,
   the round's `StopWhen` gains a condition that stops the round when the last
   step finished with tool calls and its prompt (fresh + cache-read input)
   reached the budget. A step that produced the final answer never trips it.
2. **Resume branch in the run loop** (`internal/agentcore/run.go`): when a
   round stopped at the checkpoint, the loop carries the round's messages
   (`carryRoundMessages`, tool evidence kept, provider reasoning dropped), adds
   a `[context_checkpoint]` breadcrumb to the session log, emits
   `fleet.context_checkpoint` (`used_tokens`, `resend_budget_tokens`,
   `trigger=resend_budget`, `checkpoint`), and `continue`s **without** calling
   the completion policy, without enforcement messages and without consuming
   an enforcement round. The next iteration's `checkContextPressure` compacts.
   The scheduler SSE forwards both events as `context_checkpoint` and
   `context_compacted` frames (`internal/runner/task_stream.go`).
3. **Caps**: `maxResendCheckpoints = 40` per run, tracked on the engine so the
   stop condition itself goes inert past the cap (otherwise every round would
   end after one step and the policy would read that as a finish). Past the
   cap the run is governed by the cost/token ceilings exactly as before. The
   **step cap** (`MaxIterations`) keeps bounding the whole tool loop: the
   engine counts the steps its checkpoint-ended rounds consumed — the round's
   TOTAL, including steps a resilience recovery resumed past
   (`streamRoundOutcome.completedSteps`), not just the final attempt's — and subtracts
   them from the next round's `StepCountIs`, and a pause is refused when the
   step that reached the budget also reached the cap — the cap wins the tie
   and its ordinary handling takes over. The accounting resets when a round
   ends on its own (finish or enforcement), so enforcement rounds keep their
   full budget as before (`TestRun_ResendCheckpoint_StepCapHoldsAcrossCheckpoints`).
4. **A real summarizer for scheduled runs** (`internal/agent/interactive.go`,
   `buildScheduledCompactionSummarizer`; wired in `scheduled.go`): the chat
   path's governed LLM summary, metered through `RecordUsage`, plus an addendum
   for unattended work (completed steps with their concrete results, every
   identifier the task still needs, which writes already succeeded). Nil model
   or an over-ceiling run still degrade to the deterministic placeholder. The
   summary is bought from the model the run is **currently driving**
   (`CompactionSummarizeInput.Model`, set by the engine to the fallback after a
   resilience swap), not the configured primary — a swapped run must not keep
   summarizing on a model that just failed or is circuit-open. The engine
   records the model at the round's start and at every in-round swap
   (`noteActiveModel`), so a reactive compaction inside the resilience loop is
   covered too. The interactive summarizer honours the same field.

## What did not change

- Interactive runs: no checkpoint, no resend-budget compaction — a chat's
  history is the user's to keep (`TestResendBudgetCheckpoint_ScheduledOnlyAndBudgetGated`).
- The window-pressure compaction and its `FLEET_SCHEDULED_AUTO_COMPACT` opt-in.
- The knob: `FLEET_CONTEXT_RESEND_BUDGET_TOKENS` (default 80K) is reused; `0`
  disables both the between-round trigger and the checkpoint.
- Side-effect safety: a checkpoint happens *between* completed steps (the tool
  result is already in the transcript), so nothing is replayed;
  `TestRun_ResendCheckpoint_ScheduledPausesCompactsAndResumes` pins that every
  tool step runs exactly once across six checkpoints.

## Deferred

- A per-run cap that scales with the budget or the step count (40 is a flat
  guard, chosen so a 100-step run can checkpoint every few steps).
- Rendering the `context_checkpoint` / `context_compacted` frames in the
  Operations Center run view; the scheduler SSE (`taskStreamBuffer`) forwards
  both today, and the session log carries the breadcrumbs.
