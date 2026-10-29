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
   (`carryRoundMessages`, tool evidence kept, provider reasoning dropped but
   for Gemini thought signatures, which a Gemini model gets back with the
   turn's tool calls and no other model is shown), adds a
   `[context_checkpoint]` breadcrumb to the session log, emits
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
  happens per step: between pauses every call's history is an exact prefix of
  the next call's, and a stub never changes once written, so a later elision
  rewrites only the results that left the window since
  (`TestRun_Elision_KeepsThePromptCachePrefix`, which fails if elision is moved
  into the per-step chain). A subagent runs the scheduled loop, including one a
  chat spawns, so it elides too; an interactive chat turn never does.
- **What.** Every tool result after the pinned head and before the recent
  window is replaced with its stub, when the stub is shorter: the start and end
  of the output (about 1 KiB, the whole stub at most 2 KiB), its original size,
  and a recovery action. A result whose full output the tool output boundary
  saved keeps its `artifact_path`, and its recovery is `view_file` on it. Any
  other says the call is done and should not be repeated just to see it again,
  and that only a tool that reads data should be re-run, with narrower output:
  the in-round guard's plain "Re-run <tool>" is wrong advice for a call with
  side effects, and elision stubs results routinely, not only near overflow.
  The recent window is the newest messages that fit 30% of the budget as they
  are sent, extended to at least the last two assistant turns and moved back so
  it never starts on a tool message.
- **In the provider's tokens.** The in-round guard's estimator counts about 4
  characters a token. Providers count very differently, by model as much as by
  data, and dense output (digits, punctuation, base64) costs far more per
  character: measured on comma-separated 4-digit numbers, claude-haiku-5.5
  counts 2.4 tokens per estimated token, gemini-3.8-flash 4.0 and
  gpt-6-luna-pro 7.3 (on prose, 1.5, 1.1 and 3.0). On a live Haiku run at 2.3,
  an uncalibrated "30% of the budget" window held some 60% of it, the stubs'
  savings looked half their size, and elision never freed enough. So each
  elision measures the run's own rate first: the provider's count for the call
  that fired, less the prefix measured before any history existed (#1600), over
  the estimate of the history that call carried, counted as it was sent (a
  media result, and a result or input over the hard cap, at the stub the
  in-round guard sends in its place). It sizes the window, the savings and the
  estimate below at that rate, clamped to 0.5–12, and is 1 when there is
  nothing to measure against (no floor yet, or a size that is itself an
  estimate).
- **Left alone.** Results of 2 KiB or less; media; results over the 128 KiB
  hard cap, because the in-round guard already sends a stub of the same size in
  their place on every call; and the results of `spawn_subagent` (a child run's
  answer, which only a whole new child run could reproduce) and `task_tracker`
  (the plan the finish gate holds the run to, which a summary re-announces and
  an elision does not). A2A peer tools are named per peer, so they are not kept
  by name; their stubs say to re-run only a tool that reads data. Assistant
  text, tool-call inputs, the pinned head and any summary are byte-identical,
  every result keeps its call ID (#1106), and the history is copied, never
  written in place. An elision that would save less than 2% of the budget is
  not made at all: it could not skip the summary.
- **Then.** The resent size is estimated again: the measured prefix plus the
  elided history, as sent, at the run's rate. That includes what the provider's
  count for the paused call did not yet include (the step's own assistant
  message and results, and anything appended after them). With no floor
  measured, it falls back to that count, less the stubs' savings, plus the
  paused step's output, and never below the estimator's own count of the elided
  request. On the #1600 fixture the estimate came within 0.2% of what the next
  step resent, and on a fixture counting tool output at twice the estimator's
  rate, within 3%. The summary is bought only if the estimate is not at least
  30% of the budget below the threshold: a run that elides gets room for
  several more steps before its next pause, rather than pausing again on the
  next one. When the summary does run, it is written from the history as it
  stood before this pass's stubs, so the identifiers it is asked to keep are
  not lost to a preview just made; only the kept half carries this pass's
  stubs. A result stubbed at an earlier pause is a preview by then, in the
  summary's input as everywhere else.
- **Budget.** For the resend budget it is the budget; under the floor rule the
  threshold is floor + budget, so the target is floor + 70% of the budget. For
  the window path it is the compaction point: window × the compaction
  threshold, clamped to the in-round allowance when the window checkpoint is
  the run's checkpoint (the resend budget turned off).
- **Bookkeeping.** An elision is a clean reduction like the proactive
  compaction: it resets the consecutive-compaction count, on the window path
  clears the stalled window flag, and marks the floor for re-measurement at the
  next step.
- **Visible as.** `fleet.context_compacted` with `trigger: "elision"`,
  `elision_trigger` (`resend_budget` or `window`), `elided_results`,
  `elided_bytes`, `used_tokens`, `estimated_tokens_after` and
  `history_token_rate`, plus the firing trigger's fields
  (`resend_budget_tokens` and, under the floor rule, `resend_floor_tokens` and
  `effective_budget_tokens`; or `window_size`). The session log gets
  `[context_elided] trigger=… used=… elided_results=… elided_bytes=…
  estimated_after=… token_rate=… — …`, ending with whether the summary was
  still needed; the Operations Center timeline shows it as **Older tool results
  trimmed**, and as a gap in the overview strip. A summary that follows is
  recorded exactly as before.
- **Held back** (added 2026-10-22, after the recurring-task bench in
  [Measured](#measured)). A run does not elide, and its compaction is the
  summary, which keeps the recent half verbatim:
  - while its self-audit is pending: asked for, and no `confirm_audit`
    accepted yet. That is when a run re-reads the evidence for its answer, and
    a stub of the report it is checking made the audit fail;
  - for the rest of a run that re-ran a call whose result an elision had
    stubbed: the same tool with the same arguments, in any key order, made
    after that elision. The stub cost that run a call, so eliding more is not
    saving it anything.
    - The run's new calls are told apart by position since its last context
      check (each round's start), not by call ID, because some providers reuse
      IDs. So a duplicate made before the elision does not count.
    - A re-run in a round whose history the overflow recovery compacted can
      be missed; the run then just goes on eliding.
    - A read of something that changed in between also counts, for example a
      file viewed again after an edit. That turns elision off and raises the
      budget (next bullet) for that run, nothing more.

  The server log says when either applies.
- **Budget raised after a re-run** (added 2026-10-26, after more Opus runs on
  the recurring-task bench in [Measured](#with-the-budget-raised-after-a-re-run-2026-10-26)).
  Stopping elision did not end such a run's pauses. Each re-fetch put the
  prompt over the budget again, the next summary dropped other results, and
  the model fetched those again. So from the re-run on, the run's resend
  budget is three times the configured one (`engine.resendBudget`):
  - **Why three.** The bench's twelve reports came to about 158K tokens
    resent, so twice the 80K default would pause again just as the last one
    arrived. The model's window is no bound on its own: the bench's models
    all have windows of about 1M tokens.
  - **The window cap.** The raise stops at most 30% of the configured budget
    short of the active model's input limit (the limit #1600 caps the floor
    rule at), so a call that grows by less than that can reach the pause
    before the in-round guard has to trim it. A step that adds more, such as
    a large parallel fetch, can still be trimmed first. On a 200K window the
    raise is to about 150K. A raise of less than 30% of the budget is not
    made: on a 128K window the run keeps its budget.
  - **The floor rule.** Whether it applies is still judged against the
    configured budget, so the raise never lowers the point a run pauses at.
    Under the rule the run pauses at the floor plus the raised budget, again
    at most the margin short of the input limit. Once the floor alone nears
    the limit (after a summary that kept a large recent half, say), the pause
    is at the limit itself, as it is without a raise, and the in-round guard
    may trim calls before it is reached.
  - **When it takes effect.** The run learns of the re-run at the start of its
    next round: a pause, or a finish the policy sent back. A pause that
    learns of it compacts nothing unless the prompt is already over the raised
    threshold; it still counts toward the 40-pause cap.
  - **The trade-off.** One repeat of an elided call is enough, and not every
    repeat means missing data: a file viewed again after an edit, the same
    command run twice, a bulky status polled again. Such a run also gets the
    raised budget, so for the rest of it each call may resend up to three
    times as much before the next compaction, though it pauses less often.
    Not measured.
  - **What it records.** When the budget rose, the session log gets
    `[context_budget_raised] tool=… budget=… raised_budget=… — …`, which the
    Operations Center shows as **Context budget raised**. Later
    `[context_checkpoint]` and `[context_compacted]` lines name the raised
    budget, and later events carry it as `resend_budget_tokens`. The cap
    follows the active model, so a fallback model with another window moves
    the raised budget without a new breadcrumb; the later lines show the
    value in force.
  - Runs with elision off, and runs that never re-fetch an elided result, are
    unchanged.
- **Reused call IDs** (fixed 2026-10-22). Some providers reuse one call ID
  across steps, or within one parallel step. Each result is paired with its
  own call: the nearest earlier message with calls under that ID, and within
  it the call in the result's own position. Before this, a lookup by ID took
  the last call with that ID, so a stub could name the wrong tool or stub a
  `spawn_subagent` answer.
- **Knob.** `FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY`, on by default; `0` or `false`
  turns it off and restores the summary-only behaviour. It takes
  `strconv.ParseBool` tokens and is registered with `fleet validate-config`.

On the #1600 fixture (a 140K prefix, an 80K budget, ~10K-token results), the
first pause elides seven results and needs no summary, and the next step
resends 162K tokens where the summary-only run resends 190K: 22K of carried
history over the prefix instead of 50K. Run on to 26 steps, the fixture pauses
twice, each time after a full budget over the latest floor, and never buys a
summary (`TestRun_Elision_ClearsTheBudgetWithoutASummary`,
`TestRun_Elision_LowersThePostCompactionFloor`). With an 80K budget, a 16K
prefix and ~17 KB results counted at 2 characters a token, the summary-only
run pauses six times; with elision it pauses four times and never summarizes
(`TestRun_Elision_SizedInTheProvidersTokens`). Those are fixtures, not
measurements; see [Measured](#measured) for real runs.

### Measured

On a local fleet instance, 2026-10-20, with `anthropic/claude-haiku-5.5` and a
synthetic scheduled task: one `run_python` call per seed, each printing about
17 KB of seeded random numbers, then a table of one number per seed. Each row
is one run, so model variance is in it (the step counts differ by one or two).
It is not the issue's "real recurring scheduled task"; for that shape see
[the recurring-task bench](#on-a-recurring-task-with-a-self-audit-2026-10-22)
below, still on a local instance.

| Budget, seeds | Elision | Pauses | Summaries | Prompt tokens (cached) | Completion | Cost |
|---|---|---|---|---|---|---|
| 80K (default), 30 | off | 8 | 9 | 2,599,736 (1,336,452) | 31,357 | $0.170 |
| 80K (default), 30 | on | 6 | 2 | 2,076,830 (1,248,154) | 14,987 | $0.113 |
| 16K, 14 | off | 4 | 5 | 1,096,959 (506,073) | 14,319 | $0.078 |
| 16K, 14 | on, before calibration | 4 | 4 | 875,558 (437,600) | 15,694 | $0.061 |

At the default budget, elision cut the summaries from nine to two, completion
tokens by half and cost by a third. That run used the build before the last
change, which only counts media and oversized results as sent; the task had
neither. The run measured 2.31–2.38 provider
tokens per estimated token, and elided four or five results at each pause; the
two summaries came when the stubs had piled up enough that elision alone no
longer cleared the target. A first 80K run with elision on, built before the
calibration above, elided only one or two results per pause, never freed
enough, and bought nine summaries like the run without it; that run then
failed its own self-audit, because the model would not vouch for values it had
only from compaction summaries (the run without elision passed the same
audit). The 16K pair also ran before the calibration. There, each result is
about a third of the budget, so even calibrated elision would rarely clear the
target. Even so, the floor settled near 41K instead of climbing to 62K, and
prompt tokens fell by a fifth.

The same task on other models, 2026-10-21, at the default 80K budget, each
model once with elision off and once on (the build before the rate cap below
was raised; only Luna Pro's rate reached it, and Luna Pro ran a third time
with the new cap):

| Model | Elision | Result | Pauses | Summaries | Completion | Cost | Rate |
|---|---|---|---|---|---|---|---|
| claude-sonnet-5.5 | off | success | 8 | 9 | 22,272 | $3.19 | |
| claude-sonnet-5.5 | on | success | 6 | 2 | 9,147 | $2.08 | 2.31–2.39 |
| claude-opus-5.5 | off | success | 8 | 9 | 22,474 | $6.41 | |
| claude-opus-5.5 | on | success | 6 | 2 | 9,499 | $4.21 | 2.32–2.39 |
| deepseek-v4.1-flash | off | success | 9 | 9 | 33,984 | $0.156 | |
| deepseek-v4.1-flash | on | success | 5 | 1 | 16,218 | $0.079 | 2.18–2.34 |
| gemini-3.8-flash | off | self-audit failed | 4 | 5 | 12,823 | $0.84 | |
| gemini-3.8-flash | on | self-audit failed | 2 | 0 | 11,288 | $0.78 | 3.65–3.94 |
| gpt-6-luna-pro | off | self-audit failed | 40 (cap) | 41 | 122,424 | $0.50 | |
| gpt-6-luna-pro | on | self-audit failed | 24 | 24 | 135,814 | $0.31 | 4.00 (cap) |
| gpt-6-luna-pro | on, cap 12 | self-audit failed | 15 | 14 | 73,129 | $0.18 | 4.52–6.81 |

On the Anthropic models and DeepSeek, elision cut summaries from nine to one or
two and cost by a third to a half (DeepSeek's half partly because its run with
elision made 48 tool calls to the other's 60). On every model, the share of
prompt tokens read from the provider's cache went up with elision, and no run
paused more often: Haiku 51% to 60%, Sonnet 51% to 61%, Opus 51% to 63%,
DeepSeek 72% to 84%, Gemini 13% to 16%, Luna Pro 59% to 64%. Both Gemini runs
failed on the model's own account: it stopped after 11–12 of the 30 seeds and
then failed the audit, and with elision on it had also filled in the missing
values itself; each of its three elisions freed enough. Luna Pro failed both
ways, and not for a reason compaction can fix. On a sample like this output
(17,999 characters of the same comma-separated numbers), its tokenizer counted
about 1.8 tokens a character, which puts each result near 31K tokens, close to
40% of the budget, and the two newest results the window must keep fill most of
it. Its runs paused every one or two steps either way, and what the summaries
(and, with elision, the stubs) kept was not enough for the audit to vouch for
every seed. A direct measurement of the rate per model (listed with the bounds
on `historyTokenRate`) found Luna Pro at 7.3 on that sample and 8.2 on base64,
above the rate's old cap of 4, so the cap is now 12. Rerun with the new cap,
Luna Pro's own runs measured 4.5–4.6, once 6.8: lower than the sample's 7.3,
presumably because its history also carries the model's text, tool-call inputs
and summaries, which it counts at a lower rate (not measured separately). So
the old cap had understated its history by about a tenth, once by 40%. It still
could not free enough by eliding two results each time, summarized at all but
one pause, and failed the audit the same way, with 13 seeds' values no longer
in what it was sent.

#### On a recurring task with a self-audit (2026-10-22)

The synthetic seed task above could not show one failure, because there the
model recorded each value as it went. This bench is shaped like a production
refresh:
- **The task.** A recurring scheduled task, made by the scheduler from a cron
  recurrence on a local instance: a daily store KPI refresh at the default 80K
  budget.
- **The data.** A local stdio MCP connector serves twelve stores' daily
  reports, about 25 KB of JSON each (totals, line items, inventory), the shape
  of a page-data connector.
- **The answer.** A table of revenue, orders, target, low-stock SKUs and the
  largest discount per store. The data is generated deterministically, so all
  72 cells are scored exactly.

Haiku and Sonnet fetched all twelve reports in one parallel step, answered,
and then re-checked the answer against the reports in their self-audit. Each row is one
recurring series, except fixed Haiku (4 runs, then 2 on the final build) and
fixed Gemini (a run that died on a provider error within a minute was run
again). The last four models were measured off and fixed only. "#1827" is elision as first shipped; "fixed" holds it back
during the audit and after a re-run ([What shipped](#what-shipped-1)).

| Model | Elision | Runs succeeded | Cells correct | Cost / run | Pauses | Summaries | Re-fetches | Minutes |
|---|---|---|---|---|---|---|---|---|
| Haiku 5.5 | off | 3 of 4 | 71, 72, 63 | $0.56 | 2.2 | 4.0 | 9 | 4.0 |
| Haiku 5.5 | on, #1827 | **0 of 5** | — | $0.29 | 1.8 | 1.4 | 4.8 | 2.6 |
| Haiku 5.5 | on, fixed | 6 of 6 | 72, 65, 64, 72, 72, 71 | $0.57 | 2.5 | 3.7 | 12 | 4.0 |
| Sonnet 5.5 | off | 3 of 3 | 72, 72, 72 | $2.95 | 5.3 | 6.3 | 16 | 5.1 |
| Sonnet 5.5 | on, #1827 | 3 of 3 | 71, 72, 71 | **$7.61** | 23.3 | 20.0 | 83 | 14.6 |
| Sonnet 5.5 | on, fixed | 3 of 3 | 72, 72, 72 | $2.39 | 3.7 | 4.3 | 12 | 3.9 |
| Opus 5.5 | off | 2 of 2 | 72, 72 | $3.82 | 4.0 | 4.5 | 4 | 4.7 |
| Opus 5.5 | on, fixed | 2 of 2 | 72, 72 | $4.82 | 4.0 | 4.5 | 12 | 4.9 |
| DeepSeek v4.1 Flash | off | 2 of 2 | 72, 72 | $0.29 | 23.5 | 23.5 | 90 | 23.6 |
| DeepSeek v4.1 Flash | on, fixed | 2 of 2 | 65, 72 | $0.19 | 12.5 | 12.0 | 42 | 14.8 |
| Gemini 3.8 Flash | off | 2 of 2 | 59, 65 | $2.02 | 8.5 | 8.5 | 30 | 9.7 |
| Gemini 3.8 Flash | on, fixed | 2 of 2 | 65, 68 | $1.99 | 2.5 | 2.0 | 4 | 10.4 |
| GPT-6 Luna Pro | off | 2 of 2 | 72, 72 | $0.15 | 3.0 | 4.0 | 6 | 3.5 |
| GPT-6 Luna Pro | on, fixed | 2 of 2 | 72, 72 | $0.10 | 2.0 | 2.5 | 0 | 3.3 |

As first shipped, elision stubbed the twelve reports at the pause that followed
the audit request, so the reports the audit was re-reading were 2 KB previews.
- **Haiku** concluded that "the connector truncates its output" and aborted
  every run.
- **Sonnet** re-fetched the reports three or four at a time, once through
  subagents. Each batch put the prompt over the budget again, and the next pause
  stubbed it, often with a summary too, before the audit was done. That
  repeated 15–40 times a run.

With elision held back during the audit, every pause in it bought the summary,
which keeps the recent half (and the reports) verbatim. Both models then passed.
- **Haiku** cost about what it did with elision off ($0.57 a run against
  $0.56). The cells it missed (7, 8 and 1 in three runs, almost all low-stock
  lists) are the kind it also missed with elision off (63 of 72).
- **Sonnet** cost 19% less than with it off.

For Haiku and Sonnet, elision on this task helps only after the audit is
accepted, which is where it fired in their fixed runs.

The four other models succeeded in every fixed run, as with elision off.
Their cells correct were the same or better, except one DeepSeek run (65 of 72
against 72 in both of its off runs).
- **Opus** cost more fixed than off, but its two off runs alone spanned
  $2.14–5.50, depending on whether it re-fetched to audit. Three more runs
  each way showed the gap was not noise
  ([below](#with-the-budget-raised-after-a-re-run-2026-10-26)).
- **DeepSeek** re-fetched in loops in both modes: one off run took 34
  minutes, with 40 pauses, 40 summaries and 160 re-fetches. That is the
  model's own pattern. Its fixed runs were shorter on average, but only
  because of that run: its other off run took 13 minutes, under the fixed
  average.

In the Haiku and Sonnet runs the re-run stop did not fire, because their
re-fetches came during the audit, when nothing had been elided. In the
four-model round the server log shows it fire twice (once in a DeepSeek run
that re-fetched right after its first elision) and the audit hold 10 times.
The bench cost about $73 in all.

#### With the budget raised after a re-run (2026-10-26)

Three more Opus runs each way, on the same build as the round above, made
five a side. With elision off Opus cost $4.54 a run (median $3.75); with the
holds above, $7.08 (median $5.07). Every run got all 72 cells. In each of the
three new runs with elision, elision fired at the first pause, while Opus was
still fetching the reports over several steps, before the audit was asked for.
Opus fetched the stubbed reports again, and the re-run stop turned elision
off. The run still paused at every 80K: each re-fetch put the prompt over the
budget, the summary dropped other reports, and Opus fetched those again. One
run took 19 summaries and 40 re-fetches, for $13.42. Runs with elision off
fall into the same loop now and then (one took 12 summaries and 19
re-fetches, $7.91), but less often. With all twelve reports in its history, a
call resent about 157–158K tokens (the checkpoint breadcrumbs of the runs that
fetched them in one step), twice the budget.

Then the budget raise ([What shipped](#what-shipped-1)), five Opus runs on a
build of this branch before the window cap, which does not bind on these
models' 1M windows. That build also carries what reached main since the round
above: #1842's call-pairing fixes and four unrelated changes.

| Opus 5.5, 5 runs each | Cost / run | Mean | Median | Summaries (most in a run) | Re-fetches | Minutes |
|---|---|---|---|---|---|---|
| Elision off | $5.50, $2.14, $3.75, $3.42, $7.91 | $4.54 | $3.75 | 5.8 (12) | 6.8 | 5.4 |
| Holds only (#1842) | $4.57, $5.07, $13.42, $4.47, $7.89 | $7.08 | $5.07 | 8.8 (19) | 18.4 | 8.3 |
| Holds and the raise | $3.78, $4.27, $2.11, $2.22, $2.25 | $2.92 | $2.25 | 1.0 (2) | 3.2 | 3.0 |

- **Where the raise fired.** In two of the five runs, the ones that took the
  path that looped before: elision at the first pause, then a re-fetch. After
  the raise neither run bought another summary; they cost $3.78 and $4.27.
  Every run got all 72 cells.
- **The other three.** Two fetched all twelve reports at once and never
  elided. The third elided twice and summarized once, but never fetched a
  report again.
- **Opus's path varies run to run**, so five runs are a strong sign, not
  proof.

The same build on two other models, where the raise never fired:
- **Sonnet 5.5**, 3 runs: $1.18, $4.31 and $5.21, against $2.95 off and
  $2.39 with the holds only. Two runs looped on summaries during their audit,
  while elision is held, with 10 summaries and 24 and 36 re-fetches; one of
  them got 61 of 72 cells. That loop is in the summary path and happens with
  elision off too ([Not done](#not-done)).
- **Luna Pro**, 2 runs: $0.14 and $0.17, against $0.15 off. Elision did not
  fire.

This round cost about $67, including the three extra runs each way.

On 2026-10-27, two runs each on three more models, on the branch head with the
window cap (their windows are about 1M too, so it did not bind):
- **DeepSeek v4.1 Flash**: the raise fired in both runs. Each trimmed the
  reports at its first pause and fetched six of them again; after the raise
  neither run summarized. They cost $0.03 and $0.02 in 4 and 2 minutes,
  against $0.29 off and $0.19 with the holds only (23.5 and 12 summaries a
  run). Both scored 63 of 72: they gave each store's number of low-stock SKUs
  where the prompt asks for the SKUs, so the scorer missed nine rows. Every
  count, the total of 24 and every other cell were right.
- **GPT-6.1 Sol**, new to the bench, so with no runs with elision off:
  elision trimmed seven reports at the one pause of each run, that freed
  enough, and the model fetched nothing again, so the raise did not fire.
  $0.66 and $0.65, 72 of 72.
- **Gemini 3.8 Flash**: elision fired (four times in one run, once in the
  other) with one summary each, but nothing was fetched again, so the raise
  did not fire and the runs took the same path as with the holds only. $2.59
  and $2.41, 72 and 70 cells, against $2.02 off and $1.99 with the holds only;
  the code on that path is unchanged, so the gap is the model's run-to-run
  spread.

That round cost about $6.

### Deviations from the issue

- The before/after numbers come from local instances, not a production
  recurring task: a synthetic seed task, and a recurring store-KPI task with a
  self-audit whose connector serves generated data ([Measured](#measured)).
- The breadcrumb is `[context_elided]`, as asked, but the event is the existing
  `fleet.context_compacted` with `trigger: "elision"`, so the scheduler stream
  forwards it with no change.
- "Elide down to a target" became the headroom rule above: elision alone must
  get 30% of the budget under the threshold, else the summary runs too.
- The fake-LLM acceptance run is `TestElision_LargeToolResultsNeedNoSummary_FakeLLM`
  (`internal/taskrun`): a scheduled task through the OpenRouter adapter against
  `internal/fakellm`, elision on and no summary. `fakellm` gained an opt-in
  `SizedPromptTokens` scenario field for it, since every response reported a
  fixed 10 prompt tokens before and so could never reach a resend budget. The
  other run-level tests use the agentcore scripted model, like #1600's.
- `TestRun_ResendCheckpoint_FloorAboveBudgetWaitsForABudgetOfHistory` now runs
  with elision off: it pins the floor rule with the summary compaction, and its
  fixture, run with elision on, is the acceptance test above.
- A re-fetch the model makes after reading a stub is an ordinary tool step: it
  counts against the step cap like any other.

### Not done

- Noticing a re-fetch of a result a summary dropped. The raise follows only a
  re-run of an elided call, so a run that loops on summaries alone, as Sonnet
  did during its audit above, is not helped. That loop happens with elision
  off too; it is a matter for the summary, not for elision.
- Gemini 3.8 Flash costing more with elision. Over all the bench's runs,
  four with elision cost $2.25 a run and five without $1.49. The runs with
  elision summarized less (1–2 times against 2–12) but sent more (2.4–3.9M
  prompt tokens, against 0.5–2.4M in four of the five without): a trim keeps
  more history than a summary, Gemini takes many small steps, and only
  about 15–20% of its prompt is cached. They were also more accurate (69
  cells against 64 on average; the two cheapest runs without elision stopped
  early with wrong values). The raise did not fire in any of them. Not yet
  looked into, and a second round with elision on, on all seven models, is
  still to run.
- Holding elision back during a verifier or phone-a-friend repair round. Such a
  round comes after the audit was accepted, so it is not held, though it may
  re-read evidence too. Not measured.
- A rate per result, a re-measured prefix, and a mirror of the in-round guard's
  trimming. The rate is one average over the history: when the old results that
  get elided cost the provider fewer tokens per character than the rest (prose
  first, dense data later), their savings are over-counted. The prefix is the
  one measured at the first step: a mid-run tool-roster rebuild or a fallback
  model's tokenizer that grows it is taken for history. And the rate is
  measured against the history as the guard always sends it, but not as it
  trims it to fit the model's input allowance, which happens near the window
  (the window path, not the resend budget). In each of these the estimate after
  elision can come out low; the run then pauses again a step or two later, that
  pause cannot free enough, and the summary runs: one extra pause, no more. The
  opposite also happens: when the call that fired sent more than the measured
  span holds (reasoning resent within a round, which the carried history drops,
  or a step cut off by length, whose assistant message the replay drops), the
  rate comes out high, more results are stubbed, and a summary may run that
  elision alone would have spared; never a call past the budget.
- Getting an elided result back into the history. Once a result is a stub,
  the full output is in the session log (and, when the tool output boundary
  saved it, in the workspace file the stub names), not in what the model is
  sent; an identifier deep inside it is gone unless the model wrote it down.
  The study found recall of elided output almost never used.
- Eliding tool-call inputs. Only results change; the in-round guard still
  evicts oversized inputs near the window as before.
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
