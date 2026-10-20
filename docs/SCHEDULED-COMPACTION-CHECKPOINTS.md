# Scheduled runs: compaction checkpoints inside the tool loop

**Status:** shipped 2026-09-21; the prompt-floor rule (#1600) shipped
2026-09-22; eliding stale tool results before the summary (#1742) shipped
2026-10. Design note for the change that makes the cost-aware compaction
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
checkpoint summarized 2–4 turns and the very next step paused again: one
run logged 81 checkpoint/compaction breadcrumbs for 81 tool calls, the
resent prompt going 141K → 162K → 204K across the first three.

Each pause buys one summarizer call (metered into the run, `MaxOutputTokens`
4096) and a cold prompt cache on the next step. Measured on prod luna-pro
refresh runs, 2026-09-21..22 (sched DB `logs`):

| run | checkpoints | completion tokens | cost |
|---|---|---|---|
| Page refresh A (pre-#1577 build) | 0 | 34,516 | $0.227 |
| Page refresh A | 40 | 293,022 | $0.419 |
| Page refresh A | 40 | 297,281 | $0.486 |
| Page refresh A | 40 | 344,541 | $0.800 |
| Page refresh B (pre) | 0 | 49,616 | — |
| Page refresh B | 40 | 302,288 | $0.694 |
| Page refresh C (pre) | 0 | 42,665 | — |
| Page refresh C | 40 | 312,575 | $0.776 |

Across all 42 luna-pro refresh runs in the window, completion tokens tracked the
checkpoint count almost linearly (~7K per checkpoint): runs with 0 checkpoints
sat at 18–67K, runs with 40 at 280–345K. fleetdev showed the same pattern
(one run: 38 checkpoints, 293K completion tokens; its pre-#1577 twin: 0, 10K). The
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

- Interactive runs: no resend-budget checkpoint and no resend-budget
  compaction — a chat's history is the user's to keep, and its cost is not
  this rule's business (`TestResendBudgetCheckpoint_ScheduledOnlyAndBudgetGated`).
  **Since 2026-10-14 (#1754) an interactive run does get a different
  checkpoint on the same mechanism: a window-pressure one.**

## The interactive window checkpoint (#1754)

A chat turn is one round, so its pre-round pressure check runs once while
dozens of tool steps can grow the prompt past the model's window inside it. In
production a data-heavy chat did exactly that and then failed seven turns as
"too long" on every model. The interactive engine now pauses its tool loop on
the same stop-condition mechanism, at a different threshold:

- **Where it pauses** (`engine.checkpointThreshold`): after a tool step whose
  real prompt size (fresh + cache-read input) reached the lower of the window
  compaction threshold (`FLEET_CONTEXT_COMPACTION_THRESHOLD`, default 0.90 of
  the active model's window) and the input allowance the in-round guard
  (`modelContextBudgetStep`) enforces (`windowCheckpointLimit`). On a window
  below roughly 320K tokens the allowance is the lower one: a 200K window leaves
  about 173K after the completion and provider reserves, so waiting for 180K
  would let the guard refuse the step first.
- **What happens next**: the run loop compacts at the same point (the
  pre-round check uses the clamped limit for interactive runs, so a pause
  always finds the compaction it was waiting for) and resumes the same
  conversation. Only a tool-calls finish pauses; the cap of 40 pauses per run is
  shared with the resend-budget checkpoint.
- **What the client sees**: no `fleet.context_checkpoint` event (that event is
  scheduled-only); the `fleet.context_compacted` that follows, which the chat
  renders as "Older context was summarized to make room". The session log gets a
  `[context_checkpoint] prompt N tokens reached the window-compaction
  threshold …` breadcrumb.
- **When it goes inert**: if a window compaction finds nothing to summarize
  (one enormous message), the checkpoint stops pausing until a later
  compaction — proactive or the reactive overflow recovery — removes history.
- **Scheduled runs can opt into it** (#1764): a scheduled engine uses the
  window checkpoint instead of the resend budget when the operator turns the
  budget off (`FLEET_CONTEXT_RESEND_BUDGET_TOKENS=0`) **and** opts into
  proactive compaction (`FLEET_SCHEDULED_AUTO_COMPACT=1`) — "compact only near
  the model's limit" (`engine.windowCheckpointActive`). Without it, a
  scheduled run is one round of many tool steps whose window compaction is
  checked only at round start, so a long run could reach the window before
  the next check and fall to the reactive overflow recovery. With the budget
  off and no opt-in, a scheduled run still never pauses or rewrites its
  transcript. Why an operator would choose this: with prompt-cache hits above
  80%, a resend-budget compaction (one uncached summarizer call plus a cold
  step after it) costs more than the cache-read savings it buys; on
  2026-10-15 production data it was ~$5–6 of ~$14 Argon spend.
- **What it does not change**: chat compaction still shortens the request only;
  the stored transcript keeps its history, so a later turn that starts over the
  threshold compacts again. Scheduled runs are unchanged.

Pinned by `TestRun_WindowCheckpoint_InteractivePausesCompactsAndResumes`,
`TestCheckpointThreshold`,
`TestStreamRound_ForcedCompactionClearsStalledWindowCheckpoint`, and for the
scheduled opt-in `TestWindowCheckpointActive` and
`TestRun_ScheduledWindowCheckpoint_CompactsNearTheLimitAndResumes`.
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

## Stale tool results before the summary (#1742)

### Why

Every scheduled compaction above bought an LLM summary: about 7K completion
tokens and a cold cache per pause, measured on the prod Pages refreshes in
[The prompt floor](#the-prompt-floor-1600). In a data-heavy run most of what is
resent is old tool output (7–50 KB `get_page_data` payloads), and those stayed
verbatim while they were in the kept recent half, which is what pushes the
post-compaction floor toward prefix + 2 × budget. Fleet already had a
deterministic stub for a tool result, the in-round guard's
`aggregateResultEnvelope`, but it ran only as a near-overflow backstop on a
per-request copy, so it never cut cost. The harness-design study the issue
cites (arXiv 2609.20804, §2.3, §3.2, Fig. 5) found that eliding stale bulky
observations first and summarizing only past a hard threshold had the lowest
cost per task at every window budget it tried, with comparable success and
fewer summarizer calls; making the elided content recallable added nothing.

### What shipped

`internal/agentcore/checkpoint_elision.go`, called from `checkContextPressure`:

- **When.** Only when a scheduled run's compaction fires: the resend-budget one
  (checkpoint or between rounds), or the window one on the
  `FLEET_SCHEDULED_AUTO_COMPACT` opt-in. That is the moment the history is
  rewritten anyway, so the stubs cost no extra prompt-cache miss. Nothing
  happens per step.
- **What.** Every tool result after the pinned head and before the recent
  window is replaced with its stub, when the stub is shorter: the start and end
  of the output (2 KiB in all), its original size, and the recovery action
  (`view_file` on the governed saved output when the result carried an
  `artifact_path`, else "Re-run <tool> with narrower output"). The recent window
  is the newest messages that fit 30% of the budget, extended to at least the
  last two assistant turns and moved back so it never starts on a tool message.
  Left alone: results of 2 KiB or less; media; and results over the 128 KiB hard
  cap, because the in-round guard already sends the same stub for them on every
  call. Assistant text, tool-call inputs, the pinned head and any summary are
  byte-identical, every result keeps its call ID (#1106), and the history is
  copied, never written in place.
- **Then.** The resent size is estimated again: the provider's own count for
  the step that fired, less what the stubs removed by the in-round guard's
  estimator. The summary is bought only if that is not at least 30% of the
  budget below the threshold, so a run that elides lands far enough under it
  that the next step does not pause again. When the summary does run, it
  summarizes the elided history.
- **Budget.** For the resend budget it is the budget; under the floor rule the
  threshold is floor + budget, so the target is floor + 70% of the budget. For
  the window path it is the compaction point (window × the compaction
  threshold, clamped to the in-round allowance).
- **Bookkeeping.** An elision is a clean reduction like the proactive
  compaction: it resets the consecutive-compaction count, clears the stalled
  window flag, and marks the floor for re-measurement at the next step.
- **Visible as.** `fleet.context_compacted` with `trigger: "elision"`,
  `elided_results`, `elided_bytes`, `used_tokens` and `estimated_tokens_after`,
  plus the firing trigger's fields (`resend_budget_tokens` and, under the floor
  rule, `resend_floor_tokens` and `effective_budget_tokens`; or `window_size`).
  The session log gets `[context_elided] trigger=… used=… elided_results=…
  elided_bytes=… estimated_after=… — …`, ending with whether the summary was
  still needed; the Operations Center timeline shows it as **Older tool results
  trimmed**. A summary that follows is recorded exactly as before.
- **Knob.** `FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY`, on by default; `0` or `false`
  turns it off and restores the summary-only behaviour. It takes
  `strconv.ParseBool` tokens and is registered with `fleet validate-config`.
  Interactive engines never elide, whatever the knob says.

On the #1600 fixture (a 140K prefix, an 80K budget, 16 steps of ~10K-token
results), the first pause elides seven results and needs no summary, and the
next step resends 162K tokens where the summary-only run resends 190K: 22K of
carried history over the prefix instead of 50K
(`TestRun_Elision_ClearsTheBudgetWithoutASummary`,
`TestRun_Elision_LowersThePostCompactionFloor`). That is a fixture, not a
measurement; see below for a real run.

### Deviations from the issue

- The breadcrumb is `[context_elided]`, as asked, but the event is the existing
  `fleet.context_compacted` with `trigger: "elision"`, so the scheduler stream
  forwards it with no change.
- "Elide down to a target" became the headroom rule above: elision alone must
  get 30% of the budget under the threshold, else the summary runs too.
- The acceptance tests use the package's scripted mock model, like the #1600
  checkpoint tests, not the `internal/fakellm` HTTP server.
- `TestRun_ResendCheckpoint_FloorAboveBudgetWaitsForABudgetOfHistory` now runs
  with elision off: it pins the floor rule with the summary compaction, and its
  fixture, run with elision on, is the acceptance test above.
- A re-fetch the model makes after reading a stub is an ordinary tool step: it
  counts against the step cap like any other.

### Not done

- Eliding tool-call inputs. Only results change; the in-round guard still
  evicts oversized inputs near the window as before.
- A way to recall elided output other than the stub's recovery action, which
  the study found unused.
- Eliding per step, and eliding in interactive chat (out of scope in the issue).

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
