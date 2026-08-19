# Auxiliary model-call metering (#1118)

Every `Generate`/`Stream` call fleet makes **on behalf of a run** must record
its usage somewhere visible — the run's own accounting, or an explicit,
labeled overhead ledger. Before #1118 several auxiliary calls did neither:
the compaction summarizer, the three git-metadata tools, the end-of-run
verifier, the phone-a-friend review, and the scheduled loop's `llm`
exit-condition verifier all spent tokens that appeared nowhere. That meant
`checkCeilings` and sub-agent `BudgetState` slices under-counted real spend:
"the parent ceiling is a hard wall" had unmetered leaks.

## The rule

- A call that fires **during a run, inside its governed loop's lifetime**,
  counts **against the run's ceiling**: it meters into the same
  `orchestrationState` the main loop's `OnStepFinish` feeds, so
  `Result.Usage`, the chat cost chip, `checkCeilings`, and sub-agent budget
  slices all see it. The finalize retries already worked this way
  (`FinalizeInput.RecordUsage`, #83); #1118 extends that seam to the
  remaining in-loop callers. Aux calls route through a dedicated
  `updateAuxUsage` path: they accumulate into the cumulative ceiling/billing
  totals but never overwrite the `LastStep*` per-call input-size signals
  (the context-window-fill numbers `checkContextPressure` and the chat
  context meter read) or the served-upstream attribution — those belong to
  the main loop's steps.
- A call that fires **outside or around the governed loop** — a host-side
  extra whose documented accounting deliberately excludes it from the run's
  ceilings — stays **off-ceiling** but is recorded as **labeled overhead** in
  the session log's `aux_usage` ledger. Off-ceiling is a documented choice;
  invisible is a bug.

## On-ceiling: the in-loop callers

### Compaction summarizer (`internal/agent/interactive.go`)

`summarizeDroppedMiddle` fires exactly when a run is already huge/expensive
(the prompt overflowed, or is about to). It now receives
`agentcore.CompactionSummarizeInput`, whose capabilities `agentcore.Run`
binds to the run's orchestration state (`engine.bindRunUsage`):

- **`RecordUsage`** meters the summarizer's one tool-less call into the run's
  accounting — same capability-closure contract as
  `FinalizeInput.RecordUsage`; the state never escapes `Run`.
- **`OverCeiling`** is a cheap pre-check: when the run's cost/token ceiling
  is already met, the summarizer does **not** call the model and returns the
  deterministic placeholder instead (the existing truncation path). Aborting
  is safe here because compaction still drops the middle and inserts a
  structurally-sound placeholder — the context pressure is relieved either
  way; only the summary's quality degrades. There is therefore **no ceiling
  exemption**: an over-budget run gets truncation, not a paid summary.

Scheduled runs wire no summarizer (`Deps.CompactionSummarizer` is nil) and
use the engine's deterministic placeholder — no model call, nothing to meter.

### Git-metadata tools (`internal/tools/metadata_tools.go`)

`suggest_branch_name` / `suggest_commit_message` / `suggest_pr_description`
are **model-invocable** — the model chooses how often to call them — so their
per-call `MaxOutputTokens` and 20s timeout bound one call, not the total.
Each `generateMetadata` call now meters through the **context-carried**
`tools.UsageRecorder` that `agentcore.Run` installs on the run context
(`tools.WithUsageRecorder`), attributed to the resolved metadata model's slug
so per-model price overrides (#297) price it correctly.

Why a context seam rather than a constructor argument: the tools are built by
the driver (`internal/scheduledrun.runWorker`) before the run — and its
policy, which owns the orchestration state — exists. The run context is the
one channel that reaches the tool at call time; it is the same pattern the
ask/notify handlers and the artifact collector already use, and a spawned
sub-agent's own `Run` re-binds the key to the child's state, so spend is
always attributed to the run that paid for it. Outside a governed run (unit
tests, direct invocation) the recorder is absent and the tool simply has no
run to charge.

## Off-ceiling, visibly recorded: the host-side extras

The end-of-run verifier (`internal/agent/verifier.go`), the phone-a-friend
review (`internal/agent/reviewer.go`), and the scheduled loop's `llm`
exit-condition verifier (`internal/scheduledrun/loop.go`) are one-shot
host-side re-checks layered around the governed loop. Their spend does
**not** debit the run's ceilings — that is intentional, pre-existing
semantics, not an accident of this change:

- The loop verifier's accounting model (#179) explicitly counts only the
  (dominant) worker `session.Cost` toward the across-iteration `max_cost_usd`
  ceiling; the code documented the exclusion in so many words.
- The verifier and reviewer are documented "host-side extras" that fail open
  and run at most once per run; debiting the run's ceiling from inside
  `CanFinish` could newly flip a run at its ceiling boundary into a budget
  stop after the work was already done, changing #1105/#179 semantics this
  issue did not ask to change.

What changed is visibility: each call now appends one record to the session
log's **`aux_usage`** ledger (`agentcore.LogSession.AuxUsage`, mirrored on
`models.LogSession` and carried through `convertLogSession`), persisted with
the run's transcript — the captain's-log file (both `redactLogSession` and
the size-cap truncation clone carry the ledger; the records hold only
label/model/tokens/cost, nothing redactable) and the runner's log
submission — and echoed as one host-log line:

```json
"aux_usage": [
  {"label": "end_of_run_verifier", "model": "…", "prompt_tokens": 812,
   "completion_tokens": 74, "cost_usd": 0.0031}
]
```

Labels: `end_of_run_verifier`, `phone_a_friend_review`, `loop_exit_verifier`
(exported constants in `internal/agentcore/session_log.go`). Records are
priced through `agentcore.ResolveStepCost`, so per-model overrides (#297)
apply to overhead too. Records are appended **before** verdict parsing — an
unparseable verdict still cost money. The headline session totals
(`prompt_tokens`/`completion_tokens`/`cost`) remain pure run spend; the
ledger is additive and `omitempty`, so a run with no aux calls serializes
byte-identically to before.

One loop-shape caveat: for a multi-iteration verification loop only the
surviving (last) worker session is persisted — pre-existing session handling
— so earlier iterations' `loop_exit_verifier` records survive only as host
log lines.

## Run-traceable but log-line-only

Two host-side calls ARE traceable to a specific run/conversation but fire
where no live session ledger is reachable. They are metered as the same
structured host log line the ledger-backed calls emit
(`aux model call: label=… model=… prompt=… completion=… cost=…`, logged
before output validation — a non-conforming reply still cost money). That
the log line is their ONLY record is a documented limitation, not an
oversight:

- **Error analysis** (`internal/agent/erroranalysis.go`, label
  `error_analysis`, #317): diagnoses ONE specific terminal task failure, but
  runs after that run's session was persisted, and the runner hands
  `AnalyzeTaskFailure` primitives (task prompt / error / log tail) by design
  so the runner stays decoupled from this package. Appending to the
  already-submitted session would need a store re-write seam this change
  deliberately does not add.
- **Recurring-task synthesis** (`internal/agent/recurring_task.go`, label
  `recurring_task_synthesis`, #455): `SuggestRecurringTask` synthesizes a
  proposal from a CHAT conversation transcript on user request — there is no
  run session at all. (An earlier draft of this note called this
  "recurring-task handoff summarization"; that was imprecise — the #504
  recurring-run context carry makes **no** model call, it is a deterministic
  clamp of the prior run's final answer. The model call in this file is the
  chat→task synthesizer.)

## Deliberately out of scope

Host-side maintenance calls **not tied to a specific run's budget** were left
as they are: conversation title suggestion (`title.go`), the conversation
summarizer (`summarize.go`, which already prices via `ResolveStepCost` and
returns its usage to its caller), user-memory extraction
(`memory_extract.go`), the memory-graph extractor (`memory_graph.go`),
learned-instruction distillation (`DistillLearnedInstruction`,
`learned_instructions.go`), the library-prompt synthesizer
(`library_prompt.go`), and the evals LLM-judge (`internal/evals/judge.go`).
They are per-feature host utilities with their own bounds and their own
callers (conversation- or feedback-level, not run-level); folding them into a
run ledger would attribute spend to runs that did not incur it. Named here so
the next audit does not re-litigate them one by one.

## Tests

- `internal/agentcore/aux_usage_metering_test.go` — summarizer spend and
  metadata-tool spend land in `Result.Usage` through a real `Run` (the
  metadata test was verified red on the pre-fix code: 100/20 vs 120/27);
  the seam capabilities bind to the run state and the ceiling probe flips;
  `updateAuxUsage` accumulates totals without clobbering the `LastStep*`
  main-loop signals.
- `internal/agent/aux_usage_test.go` — summarizer meters its Generate call;
  over-ceiling degrades to the placeholder with **zero** provider calls;
  verifier/reviewer records land in `aux_usage` with the right labels and
  never move the headline totals; the captain's-log FILE carries the ledger
  through both the redaction copy and the size-cap truncation clone
  (verified red on the pre-fix clones).
- `internal/tools/metadata_tools_test.go` — `generateMetadata` meters
  through the context recorder, attributed to the resolved slug.
- `internal/scheduledrun/aux_usage_loop_test.go` — the loop exit verifier
  records `loop_exit_verifier` without touching the iteration's headline
  cost; `convertLogSession` carries the ledger into the submitted
  `models.LogSession`.
