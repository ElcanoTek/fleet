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
3. **Cap**: `maxResendCheckpoints = 40` per run, tracked on the engine so the
   stop condition itself goes inert past the cap (otherwise every round would
   end after one step and the policy would read that as a finish). Past the
   cap the run is governed by the cost/token ceilings exactly as before.
4. **A real summarizer for scheduled runs** (`internal/agent/interactive.go`,
   `buildScheduledCompactionSummarizer`; wired in `scheduled.go`): the chat
   path's governed LLM summary, metered through `RecordUsage`, plus an addendum
   for unattended work (completed steps with their concrete results, every
   identifier the task still needs, which writes already succeeded). Nil model
   or an over-ceiling run still degrade to the deterministic placeholder.

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
- Surfacing `fleet.context_checkpoint` in the Operations Center run view; it is
  in the SSE stream and the session log today.
