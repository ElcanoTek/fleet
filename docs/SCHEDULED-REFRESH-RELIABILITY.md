# Scheduled refresh reliability: the answer the verifier judges, connector outages, blocked runs, staged-task cards, declared serialization keys

A production investigation of scheduled "Pages refresh" tasks (fleet
2026.10.01.6, 2026-09-24 to 10-04) found five fleet-side reasons that such
runs failed, or stopped updating a dashboard without anyone noticing. This
note records what shipped for each, what deviated from the brief, and what
was left out.

## A. The verifier judges the answer the run persists

**Problem.** After a verifier repair round ("End-of-run verification found
unresolved required actions: [report dates skipped, …]") the model appended
only the missing details. The verifier read only that latest round's closing
text (`latestRunText()` was `roundFinalText`), declared the whole report
missing again, and after three checks the run dead-lettered with `completion
verification unresolved after 3 checks`. The text the dead-letter left behind,
which concatenates every round, contained every requested item.

**What shipped.**

- `scheduledPolicy` keeps `judgedAnswer`: the answer as it stood when a model
  finish gate (the end-of-run verifier, or the phone-a-friend reviewer) judged
  it and sent the run back for repair. The repair round's closing text is
  appended to it, separated by a blank line. Both gates read that composed
  answer. The verifier's FINAL RESPONSE section and its system prompt say so.
- A new optional agentcore seam, `RunAnswerProvider`, makes the same composed
  text the run's `Result.FinalText` once the policy grants completion. So the
  answer the gates approved is exactly what the driver persists as the task
  result (and email reply), what `turn_end` hooks see, and what live clients
  are told to show (`text.replace`). It runs inside the same contained policy
  boundary as `CanFinish`.
- A round the **audit/finish enforcement** refused (a pre-audit draft) never
  enters the answer. That keeps #1563's rule that a pre-audit draft is
  superseded by the post-audit text, so the gates never judge text the result
  does not include. **Amended 2026-10-21 (#1826):** rounds refused for want
  of a passed self-audit now enter it, as described in
  [The answer given before the self-audit](#the-answer-given-before-the-self-audit-1826) below.
- When a repair round's text already contains the judged text, or is
  contained in it, or equals it once whitespace is collapsed, only the longer
  text is kept, so a model that restates its whole report does not persist it
  twice. A true supplement is appended.
- A **reviewer** (phone-a-friend) repair is different. Its round's text is the
  corrected answer, so it *replaces* the reviewed text rather than following
  the wrong one, unless the repair round produced no text.
- The verifier's repair nudge says the earlier text stays and the next is
  appended (write only what was missing). The reviewer's nudge says the next
  text replaces the answer (write the whole corrected answer).
- **A JSON answer on a task with an output schema** (amended 2026-10-19):
  when the judged answer is itself exactly one JSON value, a verifier repair
  *replaces* it too, and its nudge is the reviewer's (write the whole
  corrected answer). A supplement appended after that value made two values, and the verifier rejected the
  composition as "not exactly one JSON object" on every remaining check until
  the run dead-lettered. Manifest's deep-audit tasks hit this on most repaired
  runs from 2026-10-05. A textless repair round still keeps the judged object,
  and a prose answer on a schema task keeps the append (the schema constrains
  only the terminal output `completeRun` generates afterwards).
  `TestScheduledVerifierRepairReplacesStructuredAnswer`,
  `TestScheduledVerifierRepairAppendsProseAnswerOnSchemaTask`.
- The `(no final response text)` marker still stands in when the run has
  produced no text at all. Truncation is unchanged
  (`truncateFinalResponseForVerifier`).

**Deviation.** The brief described the persisted result as "the
concatenation of all rounds' texts". That is true of a dead-lettered run
(`cancelledResult` keeps the whole stream). A successful run persisted only
its last round's text (#1563). Making the verifier judge the concatenation
alone would have let a supplement-only answer pass and then persisted just
the supplement. So the persisted success text changed too: it is now the
judged answer plus the final round.
`TestScheduledVerifierTextlessRepairRoundSeesNoResponseMarker` (the #1570
Codex P1) became `…KeepsJudgedAnswer`. `TestScheduledReviewerRepairReverifies`
keeps its original assertion that the re-check does not carry the reviewed
answer. A textless repair round after a
rejected report is now judged on that report, which is the text the run
persists. The original P1, judging an earlier draft while persisting empty
text, cannot happen.

### The answer given before the self-audit (#1826)

**Problem.** A model that answers first is then asked for its self-audit
("Before finishing: audit …"), calls `confirm_audit`, and closes that round.
Under #1563's rule the stored answer was that closing text alone, and models
often close the audit with only a pointer back: "The table is in my previous
message." The answer it pointed to was stored nowhere: not in the task result,
and not in the session log the Operations Center transcript is built from.
Live runs on 2026-10-20/21 (a synthetic task, 30 `run_python` calls, then a
30-row table) stored no table in 4 of 8 runs on Anthropic models that passed
their audit.

**What shipped.**

- **The refusal kind is recorded.** The audit/finish enforcement now records
  whether it refused a round *for want of a passed self-audit*: the "Before
  finishing: audit …" request, or "Audit not confirmed" (the model ended a
  round without having a `confirm_audit` accepted).
  `agentcore.ScheduledPolicy.FinishRefusedForAudit` reports it. Both refusals
  happen only before the run's first accepted `confirm_audit`.
- **Those rounds' text is kept.** `scheduledPolicy.CanFinish` composes the
  closing text of every such round into the judged answer
  (`keepAnswerBeforeAudit`): the answer given before the audit, then any
  correction written while auditing. The post-audit closing text composes
  after it, exactly as a verifier repair's does (`composeRunAnswer`). Each
  text is appended, and a verbatim restatement is stored once.
- **Every such round counts, not only the first.** Take a model that
  corrected a value in a refused round, and then closed with "the corrected
  table above is final". Keeping only the first round would store the
  uncorrected table under that sentence. Before this fix it stored only the
  sentence.
- **The model is told.** Each such refusal that follows a reply says so:
  `auditAnswerNote` ("Your reply above stays part of this run's final answer
  and your closing text after the audit is appended to it, so write only what
  the audit changed or adds — do not restate the answer").
- **A JSON answer is replaced, not appended.** This applies to a task with an
  output schema whose kept answer is one JSON value (the 2026-10-19 rule
  above). The refusal then carries `auditReplaceNote` instead ("Your next
  closing text replaces your reply above, so write the complete answer").
- **The gates judge what is persisted.** The end-of-run verifier and the
  phone-a-friend reviewer judge the composed answer, and the run persists it.
  The verifier's prompt now says the FINAL RESPONSE can be the earlier
  closing text followed by what the self-audit added. A verifier repair
  composes after it like after any judged answer.
- **Other refused rounds are still superseded:**
  - a round cut off by the output limit (its text is truncated);
  - a round refused for open task-tracker items;
  - one that still owes a declared action.
- **A textless round adds nothing**, and its refusal carries no note.
- **An aborted audit drops the kept text.** `confirm_audit(success=false)` is
  the run's last word on whether that answer stands. So, while no model gate
  has sent the run back, the run persists the abort's own closing text, as
  before. Once a gate has sent the run back for repair, the kept text is part
  of the judged answer, and an abort composes after it like after any judged
  answer. "Sent back" covers a verifier that returned no valid verdict too:
  its repair already composes with the answer as it stood (above).

**Deviations from the issue.**

- **Each round's reply as its own log message.** The issue's first option was
  to persist each round's reply in the session log. That was not done: the
  final assistant message now carries the answer, so a separate copy would
  show it twice in the transcript.
- **Asking the model to restate its answer.** The issue's other option was
  not taken either. A model that does not comply would still lose the answer.
- **The append contract instead.** The fix uses the contract the verifier
  repair already has, which keeps the answer whatever the model writes next.
  That has two costs.
  - A model that restates its answer *with edits* despite the note gets one
    more copy per restatement: once as given and once per edited
    restatement. Only a verbatim restatement is kept once.
  - The core hands the policy a round's closing text. When the round's last
    step has no text, that is the text the round streamed earlier
    (`run.go`'s fallback). A round that ends with "Let me compute the values
    first." and then an empty step therefore has that sentence kept ahead of
    the real answer. That adds noise but loses nothing.
- **The task `result` column.** It is the runner's one-line summary of the
  final message (`successMessage`: whitespace collapsed, 600 runes). It now
  starts with the kept answer, so two things follow.
  - A post-audit caveat or correction after a long answer can fall past the
    cut.
  - A kept answer that starts with `{` or `[` makes the summary fall back to
    "Task completed successfully".

  The full answer is the final assistant message of the log.

**Tests.**

- `internal/agent/answer_before_audit_test.go` covers:
  - the issue's reproduction (`TestScheduledAnswerBeforeAuditIsKept`);
  - an answer after the model's own refused `confirm_audit`;
  - a correction in a later refused round;
  - every refused round composing, with a textless one adding nothing;
  - the verifier judging the composed answer, and a verifier repair composing
    after it;
  - a restatement stored once;
  - a textless round;
  - the aborted audit, and an abort after a gate sent the run back (a
    missing-actions verdict, or a verifier with no valid verdict);
  - the structured-answer replace.
- Every guard was mutation-checked against these tests.
- `agentcore`'s `TestFinishRefusedForAudit` pins which refusals count.
- `internal/taskrun/answer_before_audit_e2e_test.go` runs the issue's shape end
  to end through the fake LLM and the OpenRouter adapter. Without the fix, the
  final assistant message is the pointer alone.

**Live check (2026-10-21).** These runs used a local instance and a 6-seed
`run_python` task that ends in a table. There were seven runs in all: Haiku
5.5 four times, Sonnet 5.5 twice and Opus 5.5 once.

- Every model answered before the audit, and every audit request carried the
  note.
- Every stored answer was the table followed by the post-audit text. That text
  referred back to it ("the table above stays as it is") instead of restating
  it, so no value appeared twice.
- All 42 values were correct.
- The runs cost $0.35 in total.
- The audit request was the only refusal in every run, so the three builds
  tested took the same path. The first four runs used the first cut of the
  fix, which kept only the audit-request round. One run used the build that
  added "Audit not confirmed". The last two (Haiku and Sonnet) used the final
  build, where every audit-refused round composes.

## B. Transient MCP connect failures, and a requirements miss they explain

**Problem.** `mcp: skipping best-effort http server "pages" — … lookup
pages.elcanotek.com: no such host` (a one-minute DNS blip) and `"fast_io" — …
{"code":-32000,"message":"Auth validation temporarily unavailable. Retry."}`
skipped the server for the whole run. The EXECUTION REQUIREMENTS dispatch
check then dead-lettered the task as `non-retryable failure (terminal)` after
9 seconds. Two in a row parked the schedule.

**What shipped.**

- `mcp.IsTransientConnectError` classifies a registration failure. Transient:
  a DNS failure, a timeout or deadline, a refused, reset or unreachable
  connection, an HTTP 500, 502, 503, 504 or 429, or a JSON-RPC error
  (answered, or carried on an `id:null` response) whose message says the
  condition is temporary ("temporar…", "try again", "service unavailable").
  Not transient: a 401, 403 or other 4xx, a 501 and other 5xx, a bad URL, a
  protocol error, and a JSON-RPC message that only says "retry" or
  "unavailable" ("Invalid API key. Do not retry.", "This tool is unavailable
  on your plan") or explicitly says not to retry.
  `mcp.ConnectErrorSummary` renders a credential-free cause (never the URL, a
  header or a body). The `id:null` error is now a typed
  `*mcp.UnattributedResponseError` with the same text. It deliberately does
  not unwrap to `*RPCError`.
- `mcp.RetryTransientConnect` makes up to three attempts, 2 s and then 5 s
  apart, respecting ctx and logging each retry. It retries only *fast*
  transient failures: a timeout or deadline is not retried, because a server
  that accepts the connection and never answers already spent the whole
  request timeout (up to 2 minutes). Every pause and retried attempt is
  charged to one **15 s allowance per run** (`mcp.MaxConnectRetryBudget`),
  shared by every server in the bundle binding and the remote overlay. A
  pause that does not fit stops the retry, and a retried attempt runs under a
  deadline of what is left. It applies to bundle HTTP servers
  (`agentcore.BindMCPSelectionReport`) and to hosted connections
  (`BuildRemoteMCPOverlay`). It is **opt-in through the context**
  (`mcp.WithConnectRetry`, set once per scheduled run). Across the broker the
  allowance crosses as `ScopeSpec.ConnectRetryBudgetMs`, and the child
  reports what it spent (`ConnectRetrySpentMs`), which is charged back.
  Interactive chat turns do not set it, so a turn never waits on a server
  that is down. Stdio servers are started once, as before.
- Every failed registration is reported as an `agentcore.MCPConnectFailure`
  {server, detail, transient}. Bundle scopes report theirs over the broker
  wire (`SkippedServer.Detail/Transient`); before this change bundle scopes
  reported nothing. Hosted connections report theirs in
  `RemoteMCPOverlay.ConnectFailures`, with the bearer masked.
- The requirements check (`checkToolsAgainst`) attributes each missing name
  to a failed server. When every missing name is explained by a *transient*
  failure, the error wraps `agentcore.ErrConnectorUnavailable` (class
  `connector_unavailable`). It keeps the roster message and appends
  `server pages failed to connect this run (DNS lookup failed (no such
  host))`. A non-transient failure is named in the terminal message too.
- The runner re-runs that occurrence about 5 minutes and then about
  10 minutes later (±10%). `RequeueTaskForInfraRetry` increments
  `infra_retry_count`, never `attempt_count`, so the task's `max_retries` is
  untouched. Then the class follows the RetryPolicy (`connector_unavailable`
  is a valid `retry_on`/`no_retry_on` value; default: no retry). The
  dead-letter carries `run_outcome = connector_unavailable`, with the failed
  connectors and causes in `run_outcome_detail`, and its reason starts
  `connector unavailable after 2 infra re-run(s)`. The park breaker skips it
  in both positions — **up to three consecutive** outage dead-letters, which
  park the chain with a reason naming the connector and the streak
  ([ADR-0077](adr/0077-connector-outage-is-not-a-recurrence-strike.md)).
  Replay resets the count and the outcome. It is excluded from Sentry like
  other weather.

**Limits.** A bare `required_tools` name cannot be traced to a server whose
catalog was never fetched, so it is attributed to the outage when one exists.
DNS "no such host" stays transient (the 2026-10-02 production blip was
exactly that and cleared within minutes), so a typo'd host costs three ticks
of dead-letters, each notifying, before the bounded breaker parks it. A hung
server (timeout) is not retried within the run but is still re-run at the
occurrence level, costing one request timeout per attempt. A run whose prompt
declares no requirements is unchanged beyond the in-run retry: it still
proceeds without the skipped server.

## C. A blocked run is visible

**Problem.** A run that correctly decided not to publish recorded its reason
through `mcp_pages_record_refresh_check(outcome=blocked|source_unreachable|failed)`
and finished as plain `success`. That was 26% of refresh runs, and some
dashboards showed green for 10–14 days.

**What shipped.** The completion clause accepts an optional, generic
`blocked_when`:

```json
"completion":{"any_succeeded":["mcp_pages_record_refresh_check","mcp_pages_update_page_data","mcp_pages_update_page_data_upload"],
              "blocked_when":{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked","failed","source_unreachable"],"detail_argument":"detail"}}
```

The semantics, the validation rules and the visible effects are documented in
[CONDITIONAL-TASK-COMPLETION.md](CONDITIONAL-TASK-COMPLETION.md#a-blocked-outcome-is-visible-completionblocked_when).

- The outcome is persisted on the task row (`run_outcome`,
  `run_outcome_detail`, migration 074). The driver hands it to the runner the
  way `output_json` is handed over (`LogSession.RunOutcome`).
- The Operations Center shows an amber **Blocked** badge in the task list,
  the cards, the run history and the task detail, which also gets a
  **Blocked** row with the reason. `fleet sched task list` shows
  `success (blocked)`, and `result` starts with `[blocked]`.
- The third consecutive blocked occurrence of a recurring lineage sends its
  notification as a failure event (`Blocked 3 runs in a row — …`). The
  streak lookup runs off the terminal path, it fires once per streak, and
  nothing parks. **Amended 2026-10-23
  ([ADR-0092](adr/0092-blocked-is-a-notification-status.md)):** every blocked
  run now notifies with status `blocked`, which a `failure` filter also
  fires for; the third in a row keeps its `Blocked 3 runs in a row — …`
  message but is no longer escalated to `failure`.

**Deviation.** The brief's example had no `detail_argument`; fleet must not
assume the recording tool has a `detail` argument (Pages semantics), so the
explanation is opt-in by name. Without it the detail is `<argument>=<value>`.

## D. Task-staging approval cards wait at least a day

A `manage_tasks` card that would have replaced a task's prompt previously sat
for a shorter global timeout, was auto-denied, and the old prompt kept running.
`schedule_task` and `manage_tasks` cards only stage a change and have no side
effect while pending. `resolveTimeoutSeconds` gives them a 24-hour floor
(`stagedTaskApprovalTimeoutSeconds` = 86400, matching the 86400s global default
for `FLEET_APPROVAL_TIMEOUT_SECONDS` even if an operator lowers the global
default; per-conversation and global admin overrides allow up to 604800s / 7 days)
unless the conversation's or global timeout is longer. A bundle's per-tool
`critical_tool_timeouts` entry for the tool still wins. The promote-to-task
card is a `schedule_task` card, so it gets the 24-hour floor too.

## E. A declared serialization key

`EXECUTION REQUIREMENTS` may carry `serialization_key`
(`^[A-Za-z0-9_.:/-]{1,200}$`, refused at save otherwise). `models.NewTask`,
the one constructor every create path goes through (API create and batch,
A2A, import, chat `schedule_task`, the scheduled `create_task` tool, triggers
and recurrence spawns), takes the declared key when the task has no explicit
one. An explicit key always wins.

**Deviation: creation only.** The key is documented as immutable after
creation ([TASK-SERIALIZATION.md](TASK-SERIALIZATION.md)): the edit path
never touches it, because the claim gate's mutual exclusion is reasoned
about per row. That invariant is kept. A prompt edit that adds the
declaration (API update, `manage_tasks`) does not change the edited row's
key. It takes effect on the next occurrence, which is created from the
edited prompt. An import that *replaces* a definition keeps its existing
behavior (the record's key, or none).

## Tests

- **A:** `scheduled_completion_test.go` holds the production reproduction
  (`TestScheduledVerifierRepairSupplementPassesOnCombinedAnswer`, which
  reproduces the 3-check dead-letter without the fix), the textless-run
  marker, the reviewer re-verification (replace),
  `TestComposeRunAnswer` (append, dedupe, replace) and
  `TestScheduledVerifierRepairThatRestatesTheReportPersistsItOnce`. `mode_parity_test.go` holds
  `TestRunAnswerProvider_ComposedAnswerIsTheResult`.
- **B:** `mcp/transient_test.go` (the classifier, including "Do not retry", a
  plan-unavailable message and a 501 as terminal; the retry against a flaky
  handshake server; no retry on a timeout; the shared run budget),
  `mcpbroker` (`TestClientServer_ScopeCarriesTheRunConnectRetryBudget`),
  `agentcore/mcp_selection_degrade_test.go`,
  `scheduledrun/requirements_outage_test.go`,
  `sched/storage/run_outcome_test.go` (including the three-in-a-row park)
  and `runner/refresh_reliability_test.go`.
- **C:** `models/execution_requirements_blocked_test.go`,
  `agent/completion_blocked_test.go`,
  `scheduledrun/requirements_completion_test.go`, the storage and runner tests
  above, `admincli/sched_task_test.go`, and the web `taskDisplay` and
  `LogViewer` tests.
- **D:** `httpapi/approval_timeout_test.go`.
- **E:** `models/execution_requirements_blocked_test.go`
  (`TestNewTaskTakesTheDeclaredSerializationKey`).
