# Scheduled runs: compaction checkpoints inside the tool loop

**Status:** shipped 2026-09-21; the prompt-floor rule (#1600) shipped
2026-09-22. Design note for the change that makes the cost-aware compaction
(#1534) recur inside a scheduled run's tool loop and gives scheduled runs a real
compaction summary.

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
   When the prompt floor crowds the budget the threshold is floor + budget
   instead — see [The prompt floor](#the-prompt-floor-1600) below.
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

## The prompt floor (#1600)

### What happened

From the day the checkpoint shipped, every long scheduled Pages refresh on prod
and fleetdev paused on **every** tool step until the 40-checkpoint cap. The
predicate compared the whole request with the budget, but much of that request
is not history at all: the system prompt (default.md + persona ≈ 6K tokens), the
Pages tool catalog (44 tools, 135 KB of descriptions and input schemas ≈ 34K
tokens), the other tool schemas and the 5–16 KB task prompt. Add the recent half
of history that a compaction keeps verbatim (7–50 KB `get_page_data` results)
and the request never got back under 80K, however much was summarized. Each
checkpoint summarized 2–4 turns and the very next step paused again: task
`5af5cbbf` logged 81 checkpoint/compaction breadcrumbs for 81 tool calls, the
resent prompt going 141K → 162K → 204K across the first three.

Each pause buys one summarizer call (metered into the run, `MaxOutputTokens`
4096) and a cold prompt cache on the next step. Measured on prod luna-pro
refresh runs, 2026-09-21..22 (sched DB `logs`):

| run | checkpoints | completion tokens | cost |
|---|---|---|---|
| husqvarna `978891fb` (pre-#1577 build) | 0 | 34,516 | $0.227 |
| husqvarna `5af5cbbf` | 40 | 293,022 | $0.419 |
| husqvarna `b07c8770` | 40 | 297,281 | $0.486 |
| husqvarna `561b0153` | 40 | 344,541 | $0.800 |
| twc-client-overview `fee78c81` (pre) | 0 | 49,616 | — |
| twc-client-overview `e090d44b` | 40 | 302,288 | $0.694 |
| brookfield `8ce3dbc0` (pre) | 0 | 42,665 | — |
| brookfield `89afe409` | 40 | 312,575 | $0.776 |

Across all 42 luna-pro refresh runs in the window, completion tokens tracked the
checkpoint count almost linearly (~7K per checkpoint): runs with 0 checkpoints
sat at 18–67K, runs with 40 at 280–345K. fleetdev showed the same pattern
(`957849e8`: 38 checkpoints, 293K completion tokens; `f572133f`: 0, 10K). The
summaries also threw away the model's two most recent turns on every step.

### The rule

`engine.effectiveResendBudget` in `internal/agentcore/engine.go`:

- **Floor points.** The run's first step (the task prompt, no history yet) and
  the first step after every compaction (resend budget, window pressure or the
  reactive `context_length_exceeded` recovery). The step's resent size
  (`InputTokens + CacheReadTokens`) is recorded as the **floor** in the stream's
  `OnStepFinish`, which fantasy runs before it evaluates `StopWhen`, so a floor
  step is judged against the floor it just set and never pauses on its own size.
- **The prefix** is the smallest floor measured so far: the part of every call
  no compaction can shed (system prompt, tool schemas, pinned head).
- **While the prefix is at most half the budget, nothing changes:** the
  checkpoint fires once a step's resent size reaches the budget.
- **Past that, the budget applies to the history alone:** the checkpoint fires
  once a step resends `floor + budget` or more, where the floor is the one
  measured at the latest floor point. Every pause therefore waits for a full
  budget of new history since the last compaction.
- The **pre-round resend-budget compaction** in `checkContextPressure` uses the
  same threshold. At the plain budget it would compact at every round start
  while the floor alone is over the budget.
- **History guard.** There is no pause when fewer than 4 messages follow the
  pinned head (`minResendCheckpointHistory`), counting the round's input and the
  steps' own messages. With so few, the compaction would trade one exchange for
  a summarizer call.
- **Observability.** The first time the prefix is found to crowd the budget,
  the run writes one `[context_checkpoint_floor] floor=… budget=…
  effective_budget=…` session-log breadcrumb. After that, and only while the
  rule applies, the `fleet.context_checkpoint`, `fleet.context_compacted` and
  `fleet.context_pressure` resend-budget payloads carry `resend_floor_tokens`
  and `effective_budget_tokens`, and the `[context_checkpoint]` and
  `[context_compacted]` breadcrumbs name the floor. Under the plain budget the
  payloads and breadcrumbs are byte-for-byte what they were.

### Why half the budget, and why the prefix decides

Under the plain rule, a call right after a compaction still carries the prefix,
the summary and the kept recent half. The next pause therefore comes after
roughly `(budget − prefix)/2 − summary` tokens of growth. That is a quarter of
the budget, less one summary, when the prefix takes half; one or two steps as
the prefix nears the budget; and every step once it passes the budget. Half is
where the interval drops below a quarter of the budget, so that is where the
rule switches.

The switch is decided on the **prefix**, not on the latest floor. A
post-compaction floor contains the kept recent half, and with large tool
results it can exceed the budget on its own. If that could switch the rule, a
small-prefix run would move to the floor rule because of its own history, and
its prompt would ratchet far past the budget. Taking the minimum also means a
larger post-compaction measurement can never push the prefix up.

The trade-off is stated plainly because it is real. Under the floor rule the
per-call prompt is **not** held under prefix + budget. Each compaction keeps the
recent half, and that half becomes part of the next floor, so over many
compactions the pause point settles near prefix + 2 × (budget + summary). What
the rule buys is pauses: a handful per run instead of 40. The cost is more
resent tokens, most of them cache reads, where every pause was a summarizer call
and a cold cache. A simulation of an 81-step run (80K budget, 3K summaries, 30
seeded runs per cell, the same compaction split as `proactiveCompact`) shows the
shape. It is a model, not a measurement:

| prefix | results 1–4K tokens: pauses before → after | results 2–12K tokens: pauses before → after | identical runs |
|---|---|---|---|
| 10K–40K | unchanged (5–10) | unchanged (16–30) | 30/30 |
| 50K | 14.5 → 2 | 39.4 → 6 | 0/30 |
| 60K | 23.6 → 2 | 40 → 6 | 0/30 |
| 90K | 40 → 2 | 40 → 6 | 0/30 |
| 140K | 40 → 2 | 40 → 6 | 0/30 |

At a 140K prefix with 2–12K results, the total resent across the run rises from
18.5M to 20.6M tokens, and the peak call falls from 512K (the cap exhausted,
then unbounded) to 347K.

### Deviations from the issue

- The issue suggested a history floor of 6 messages. The guard uses 4.
- The issue suggested a `[context_checkpoint_disabled]` breadcrumb. The
  checkpoint is re-based, not disabled, so the breadcrumb is
  `[context_checkpoint_floor]`. There is no separate event: the fields ride on
  the resend-budget events above.

## What did not change

- Interactive runs: no checkpoint, no resend-budget compaction — a chat's
  history is the user's to keep (`TestResendBudgetCheckpoint_ScheduledOnlyAndBudgetGated`).
- The window-pressure compaction and its `FLEET_SCHEDULED_AUTO_COMPACT` opt-in.
- The knob: `FLEET_CONTEXT_RESEND_BUDGET_TOKENS` (default 80K) is reused; `0`
  disables both the between-round trigger and the checkpoint.
- Side-effect safety: a checkpoint happens *between* completed steps (the tool
  result is already in the transcript), so nothing is replayed;
  `TestRun_ResendCheckpoint_ScheduledPausesCompactsAndResumes` pins that every
  tool step runs exactly once across five checkpoints.
- The floor rule (#1600) keeps `maxResendCheckpoints`, the step-cap tie rule and
  its accounting, and the resume branch exactly as they were. A small-prefix run
  pauses after exactly the steps that reached the plain budget, with the
  pre-#1600 payload and breadcrumb
  (`TestRun_ResendCheckpoint_SmallPrefixKeepsThePlainBudget`). A 140K floor
  against an 80K budget runs without pausing until the history has grown by a
  budget, then pauses once, and does not pause again after the post-compaction
  re-measurement
  (`TestRun_ResendCheckpoint_FloorAboveBudgetWaitsForABudgetOfHistory`).

## Deferred

- A per-run cap that scales with the budget or the step count (40 is a flat
  guard, chosen so a 100-step run can checkpoint every few steps).
- Rendering the `context_checkpoint` / `context_compacted` frames in the
  Operations Center run view; the scheduler SSE (`taskStreamBuffer`) forwards
  both today, and the session log carries the breadcrumbs.
- Re-measuring the floor when the tool roster is rebuilt mid-run (an MCP load)
  or a fallback model with a different tokenizer takes over (#1600). A floor
  point needs a step with no new history to measure, and neither event
  provides one, so the next compaction is what re-measures it.
