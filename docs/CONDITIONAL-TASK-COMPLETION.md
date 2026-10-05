# Conditional scheduled tasks

A scheduled task can make an action conditional: create an inventory item only
if it is absent, commit files only if they changed, or import a record only if
it has not already been processed. The end-of-run verifier receives bounded
structured evidence from tool arguments and results, separately, in addition
to call success. Previously it only saw names and success booleans, so it could
demand an action the task's selected branch forbade.

All conditions, required actions and prerequisites come from the original task.
Fleet does not assign completion meaning to a connector's field names or values.
A claimed condition cannot replace missing prerequisite calls or failed checks.
For long tasks, truncation retains the opening identity and closing stop rules.

The evidence projection is structural rather than an application field list:

- It accepts JSON objects and the standard MCP text-content wrapper, recursively
  retaining short identifier-like strings (ids, statuses, dates, e-mail
  addresses), exact numbers, booleans and nulls under JSON Pointer field
  paths. Literal dots stay in the key, so `/totals/rows.revenue`
  cannot collide with `/totals/rows/revenue`. Arbitrary wrapper and field names
  work alike. Short scalar arrays (including empty lists and date bounds) remain
  whole; arrays of objects and oversized arrays are omitted.
- The existing shared secret scrubber runs before extraction. Credential-bearing
  subtrees (including dotted credential keys), short free text, URLs and bulk
  arrays are omitted. Bulk text — multi-line or over the scalar limit: what
  `run_python` or `bash` printed, a file body — is not retained verbatim; in a
  tool RESULT, at most two bounded excerpts per projection (head … tail, URLs
  replaced, at most 600 characters) are recorded under `<path>#excerpt`, so a
  read-only check the model ran is visible to the verifier instead of
  vanishing. Arguments never carry excerpts: they are requested intent, not
  proof, and an emailed body or a script source would only be noise. Separate `arguments_omitted` and
  `result_omitted` flags expose omissions. No tool-result text becomes a
  verifier instruction or an authorization grant; the verifier is told excerpts
  are the tool's own output.
- A connector call made through the `tool_call` bridge is recorded under the
  connector tool's own name with its own arguments and `wrapper: "tool_call"`,
  not as a `tool_call` record whose real target is buried in the arguments.
- Input is limited to 1 MiB; each projection is capped at 4 KiB, 64 fields,
  256 visited entries and eight nested object levels. Scalar arrays are limited
  to 32 elements and share the same byte cap. Sorting makes selection
  deterministic. Unsupported, malformed or omitted evidence remains unknown.

The core records complete, redacted tool results before making the 4,000-byte
UI preview. The verifier reads that transcript, not the preview. Its projection
visits enclosing scalar fields before deeper profiles so a large nested profile
does not crowd out the enclosing outcome/version fields.

The verifier's third input is the run's final response — the closing assistant
message, bounded and delimited as its own section. After a repair round it is
the answer the verifier sent back followed by what the repair added, which is
also the text the run persists. A task requirement to
report/summarize/state something in the run's own output is satisfied when that
message contains the content; deliverables that require a tool call (email
send, page write, file upload, ...) are still only satisfied by that call. The
message is evidence, never instructions, and an absent one is passed as an
explicit `(no final response text)` marker so a missing report stays flaggable.

The verifier remains a model-based check, not deterministic proof of a business
workflow. It runs at most three times: the initial check and two repair
reviews — the cap counts every verification, including the re-check of a
reviewer-forced phone-a-friend repair, which cannot buy a fourth call. Missing
actions keep completion blocked. The third check that still reports missing
actions returns `ErrCompletionUnverified` directly through the governed core,
preserving partial work, usage and the completed-action count. It does not ask
the model to abort or run more tools, because an audit abort may be refused
after all committed writes succeeded. A verifier that *answered* with missing
actions remains a terminal failure under the existing retry policy, never a
successful completion.

A verifier call that produced no verdict is retried once after a short pause
(#1602). What happens if the retry also fails depends on *how* it failed, and
on the audit:

- **An outage** (a timeout, a provider failure) says nothing about the run.
  If both attempts ran and both were outages while the run's own context was
  still live (a run deadline that expired mid-check does not count, nor does a
  malformed first verdict), this run's **own** `confirm_audit` passed, at least one audit-gated tool
  executed successfully, and no audit-gated tool has a failed attempt that a
  later success did not supersede, the
  run succeeds with a
  `completion_unverified_verifier_error` warning. The warning is recorded in
  the session log and at the head of the task's terminal message; the run is
  not dead-lettered on the verifier's own outage. The phone-a-friend reviewer
  already failed open on its errors. Only a later success supersedes a
  failure. A success of the same tool does, unless its records (`deal_id` /
  `deal_ids`) provably miss one the failed call targeted. A success of a
  same-server alias twin (`critical_tool_aliases`) does only when it wrote
  every record the failed call targeted: both calls' recorded arguments are
  complete and name a `deal_id` / `deal_ids` binding. Later successes resolve
  a failure once they have written every record it targeted, in one call or
  several. A failed retry supersedes nothing, and neither does a
  twin that wrote another record, names no record, has incomplete evidence, is
  a bare-suffix name, or sits on another server or client variant. Page twins
  addressed only by slug therefore never supersede each other, by decision:
  supersession trusts only the record-binding contract the audit gate already
  uses, and fleet does not special-case one server's identifier.
- **A malformed verdict** (the verifier answered, but with prose, invalid
  JSON, no explicit `missing_actions` array, or an empty reply) is a content failure, not an
  outage. A degraded verifier model must not quietly become auto-success, so
  the check is spent as before, and the third ends the run
  `ErrCompletionUnverified`.
- **The premise does not hold** (a failed critical call is on the record, no
  critical call landed — the audit alone is the model grading itself — or
  no audit ran in this run's policy, as for a delegated sub-agent, whose
  policy skips the self-audit ritual). Even an outage spends the check. Today
  sub-agents do not run the verifier at all; the rule keeps it that way if
  they ever do.

Each check is therefore at most two metered verifier calls.
Its transcript records `completion_unverified` and explains that completed
external actions have not been rolled back; the dead-letter reason also names
the connector calls that succeeded this run (or says none did), so an operator
reading it knows what already went out before rerunning. The verifier and repair instructions
request read-only checks when evidence of an existing action is missing, rather
than asking for that successful mutation again. Each call is metered
in auxiliary usage. Task authors and bundles still own workflow contracts.

An explicit `confirm_audit(success=true, critical_actions=[])` now records
completion without inventing a future mutation. It activates the
typed gate with no new commitments and therefore authorizes no new critical
calls. Existing outstanding commitments remain outstanding. Missing/null
successful-audit declarations still fail. Explicit terminal audit aborts retain
the failed run outcome and skip the driver reviewers instead of re-demanding
abandoned actions.

## Copyable execution prerequisites

Prompt producers may include one literal line followed by one JSON object:

```text
EXECUTION REQUIREMENTS (JSON):
{"mcp_servers":["inventory"],"required_tools":["mcp_inventory_inspect"],"network":true}
```

- **Validated when the task is saved (#1601)**, as well as at dispatch (below).
  A malformed declaration (a duplicate
  marker, bad JSON, a server or tool name outside `^[a-zA-Z0-9_.-]{1,200}$`, too
  many names) is refused when the task is saved. That covers:
  - create, edit, clone, import (HTTP and CLI) and batch;
  - a chat `schedule_task` / `manage_tasks` call, refused before its approval
    card is staged.

  The message names the field, index and identifier, e.g. `invalid server or
  tool identifier "fast_io + fastio_helpers" in mcp_servers[0]; allowed
  ^[a-zA-Z0-9_.-]{1,200}$`. A missing, blank or oversized line after the
  marker and a duplicate marker each have their own message.
- **Dispatch and save share one parser** (`models.ParseExecutionRequirements`),
  so a prompt that saved cleanly never fails this check at dispatch. The one
  exception is a webhook or email trigger run: its prompt is rendered from the
  event at run time and is not re-validated on insert, so a rendered
  declaration can still fail here (loudly, before any model or tool work).
  Whether the named servers and tools exist is still decided at dispatch.
- **Tasks saved before the check.** A recurring task saved before it that
  still carries a malformed line dead-letters once and parks its chain at
  once, not after two occurrences
  ([ADR-0073](adr/0073-malformed-requirements-park-on-first-dead-letter.md)).
  The park records why, and the Operations Center says the schedule stopped.
  The failure notification says so too, except when a transient database
  error defers the park to the reconciliation sweep, which sends none.
  - A plain replay is refused, since it would rerun the same line.
  - `fleet sched dlq replay --prompt-file <file> <task_id>` replays the
    occurrence with a corrected prompt, keeping its schedule and task memory
    ([DEAD-LETTER-RECURRENCE.md](DEAD-LETTER-RECURRENCE.md)).
  - Editing the finished task starts a one-off run.

  Imports check live (pending/scheduled) rows only; terminal history is
  preserved verbatim.

Fleet checks this optional declaration at **dispatch**, before model execution.
A sealed task/global lockdown produces an actionable network error. After the
run's MCP scope and remote overlay are opened, missing advertised servers/tools
produce an actionable roster error. Tools may be native names, server tool names,
or Fleet's full `mcp_<server>_<tool>` names; full names avoid ambiguity. Model
resolution already happens before the run and remains mandatory.

**A connector outage is not a roster error.** A connect failure is
*transient* when it is a DNS lookup failure, a timeout, a refused, reset or
unreachable connection, an HTTP 500, 502, 503, 504 or 429, or a JSON-RPC error
whose message says the condition is temporary ("temporarily unavailable",
"service unavailable", "try again"; never one that says "do not retry", and
bare "retry" or "unavailable" are not enough). A scheduled run retries a
server's registration that fails *fast* and transiently — every transient
cause except a timeout, which has already spent the whole request timeout —
up to three times, 2 s and then 5 s apart, before the server is skipped. All
of a run's retries share one 15 s allowance (pauses and retried attempts), so
the retry never adds more than that to a run however many servers fail. An
interactive chat turn tries once. When every missing name is then explained by
a server that failed to connect *transiently* in this run (a timeout
included), the dispatch error is the same roster message followed by
`server <name> failed to connect this run (<cause>)`, and it is classified
`connector_unavailable`, not `terminal`:

- The runner re-runs the same occurrence about 5 minutes and then about
  10 minutes later. These re-runs do not spend the task's `max_retries`
  (`infra_retry_count` counts them).
- If the connector is still down after that, the class follows the task's
  `retry_policy` like any other (by default: dead-letter). The dead-letter is
  recorded with run outcome `connector_unavailable` and its reason starts
  `connector unavailable after 2 infra re-run(s)`. It does **not** count toward
  the two-consecutive-dead-letters park breaker
  ([ADR-0077](adr/0077-connector-outage-is-not-a-recurrence-strike.md)), in
  either position — up to a bound: the **third consecutive** occurrence that
  dead-letters on a connector outage parks the chain, with the reason
  `A required connector has been unreachable for 3 consecutive occurrences:
  <connector> (<cause>). …`, because a typo'd or decommissioned host, or a
  closed port, fails the same way forever.
- A server that is not configured or not selected, a tool name no connected
  server provides, and a connect failure that is not transient (a 401/403, a
  501, a bad URL, a protocol error, a JSON-RPC error that does not say it is
  temporary) stay the terminal roster error they were. A
  missing tool is traced to the failed server by its full
  `mcp_<server>_<tool>` name; a bare name cannot be traced to a server whose
  catalog was never fetched, so it is attributed to the run's transient
  outage as a whole when there is one.

**`serialization_key`** (optional) declares the task's mutual-exclusion key
([TASK-SERIALIZATION.md](TASK-SERIALIZATION.md)), e.g. `"pages:<slug>"`. It
must match `^[A-Za-z0-9_.:/-]{1,200}$`; anything else is refused at save. A
task **created** without an explicit `serialization_key` takes the declared
one, whatever the write path — the API, chat `schedule_task`, a prompt pasted
into the Operations Center form, an import, or a recurrence spawn. An explicit
task value always wins. The key stays immutable on an existing row, so a
prompt edit that adds the declaration takes effect from the next occurrence
(which is created from the edited prompt), not on the edited row itself.

`"roster":"required_tools_only"` (#1603) additionally narrows the run's MCP
roster to the tools `required_tools` names:

```text
EXECUTION REQUIREMENTS (JSON):
{"mcp_servers":["pages"],"required_tools":["mcp_pages_get_page_data","mcp_pages_record_refresh_check","mcp_pages_update_page_data_upload"],"roster":"required_tools_only"}
```

- **What registers.** Each selected server's Gate-2 allowlist is intersected
  with the required tools, which are resolved the same way as the check above.
  A server none of whose tools is required registers nothing: here `fast_io`,
  `fastio_helpers`, and every Pages layout, template or delete tool. Gate-2 is
  exhaustive and exact for the run: a server is governed only by an entry under
  its own registered name. A server the dispatch roster did not include
  registers nothing and never inherits a narrowed server's entry. That covers a
  `<server>_<account>` seat loaded mid-run with `mcp_load_servers(client=…)`
  and a server whose name merely extends a narrowed one.
- **What does not change.** Native tools stay (`confirm_audit`,
  `task_tracker`, bash, Python, the file tools). The live-registry section of
  the system prompt follows the roster.
- **No lost tools.** Narrowing never removes a required tool the manifest
  allowlist permits, because the check above has already proved it is in the
  roster. A `completion.any_succeeded` tool is kept the same way even when
  `required_tools` does not list it, so a declared predicate stays reachable.
  Narrowing only removes; it never grants a tool the allowlist denies. On the
  local MCP path the check reads the advertised catalog, so a required tool the
  manifest allowlist denies passes it and is still not registered.
- **Audit declarations.** `confirm_audit` refuses a declaration that names an
  MCP tool the run did not register and that no registered tool can stand in
  for, and that audit registers nothing. A typed `critical_actions` entry is
  accepted when the run registered that exact tool, or a same-server alias twin
  (`critical_tool_aliases`) or approved substitute (`critical_tool_substitutes`)
  of it: the tools that would discharge it. A batch entry (`deal_ids`) takes an
  alias twin only, since a substitute cannot consume a batch approval. The
  confirmation names the registered stand-in to call. Legacy
  `critical_actions_being_unblocked` text is accepted when its critical suffix
  shares an alias class with, or is substituted by, a registered tool's suffix.
  The model re-audits with tools from its list. Accepting any other declaration
  would create an approval nothing could discharge: the call answers
  `tool not found`, so finish would be refused until the run failed.
- **Removed tools.** A call to a removed tool is answered `tool not found`.
  The run log carries one `[roster] required_tools_only: N mcp tools
  registered` breadcrumb.
- **Unknown values.** Any other `roster` value, the empty string included, is
  a dispatch error. Without the key (or with `null`), the roster is exactly as
  before.
- **Where it is enforced.** The narrowing is applied in the run's own tool
  registration (Gate-2). The credential-owning MCP broker still authorizes
  against the manifest's allowlist (ADR-0042) and does not know about the
  narrowing, so a parent-side bug could at worst give a run the normal manifest
  roster back, never a tool outside it.

The point is cost and safety. A Pages data refresh resent about 34K tokens of
tool schemas it never used on every step. One production data refresh
declared the layout-mutating `deploy_page_upload` in its audit, a tool a data
refresh must never reach, and ended in error when it could not discharge that
declaration. On a narrowed roster the tool is not registered, so it cannot be
called, and `confirm_audit` refuses a declaration that names it. Fleet still
interprets nothing about the tools: the list and the narrowing are the
producer's contract.

This declaration only restricts a run. It cannot enable network, bypass the
broker, select credentials or override an administrator's allowlist. It does not
prove endpoint reachability, per-account authorization or source completeness;
the executing workflow must still check those. Unknown companion metadata is
ignored for forward compatibility, including producer labels such as `mode`.
Malformed/duplicate declarations fail closed. Ordinary prompts with no marker
keep their existing behavior.

## Deterministic completion predicate (#1602)

For many workflows, the producer of the prompt knows exactly which tool
executions mean "done", and that knowledge is not a matter of judgement. A
Pages refresh either created a new version (`update_page_data`,
`update_page_data_upload`) or recorded why it did not (`record_refresh_check`,
the no-update branch the prompt itself calls "a complete run, not a failure").
The model verifier still read the numbered publish steps as unconditional.
On the 2026.09.22.4 build it was the largest single cause of dead-lettered
page refreshes, and every one of those runs had the right outcome already
recorded on the Pages side:

| run | verifier demanded | what actually happened |
|---|---|---|
| page A | "publish_managed_data_update … update_page_data_upload" | run recorded `blocked`; page correctly untouched |
| page B | "update_page_data publish … expected_version=N" | no new coverage; `record_refresh_check(source_not_updated)` written |
| page A (next day) | "build complete payload … update_page_data_upload" | blocked branch, recorded |
| page C | — (the verifier call itself timed out) | dead-lettered after three checks |
| page D | a wording nit in the report | data published |

The producer can declare that knowledge in the same requirements object:

```text
EXECUTION REQUIREMENTS (JSON):
{"mcp_servers":["pages"],"required_tools":["mcp_pages_get_page_data","mcp_pages_record_refresh_check","mcp_pages_update_page_data_upload"],"completion":{"any_succeeded":["mcp_pages_update_page_data","mcp_pages_update_page_data_upload","mcp_pages_record_refresh_check"]}}
```

**At dispatch**, `completion.any_succeeded` names are validated and resolved
the way `required_tools` names are:

- They may be native names, bare server tool names, or full
  `mcp_<server>_<tool>` names. The same identifier rule applies, and at most
  200 names are allowed. **Use full names.** A bare name resolves on every
  server that exposes it, so `record_refresh_check` would be satisfied by a
  success on any Pages server or client-variant seat in the roster, not just
  the one the task is about.
- A name that is not in the run's tool roster is the same actionable dispatch
  error as an unavailable required tool (`completion tool <name>`).
- A malformed clause fails closed, like any malformed declaration. That covers
  a bad identifier, a clause that is not an object, and `any_succeeded` that is
  not an array of strings.
- An absent, `null`, or empty clause declares nothing. So does a clause holding
  only keys this Fleet does not know.

**At finish**, once the audit/finish enforcement has cleared, the scheduled
policy reads the same tool-execution records the verifier reads, with the same
success classification. Bridged `tool_call` connector calls count under their
own name.

- **A listed tool has a successful execution.** The run is complete:
  - the end-of-run verifier and the phone-a-friend review are skipped (zero
    model calls, and no verifier entry in `aux_usage`);
  - the session log gains a
    `[completion_predicate] satisfied by <tool>` breadcrumb;
  - the run emits a `fleet.completion_predicate` event, which the scheduler
    stream forwards as a `completion_predicate` frame.
- **The predicate replaces only the model gates, never the audit.** An
  outstanding declared commitment still blocks finishing, because the
  predicate is consulted only after audit/finish enforcement has cleared.
- **No listed tool succeeded** (failed, blocked, or never called). The
  verifier runs exactly as before.
- **No clause.** Nothing changes.

Fleet still interprets nothing about these tools. The list is opaque names
chosen by the producer, like `required_tools`. Fleet does not know what
`record_refresh_check` means, and the bundle and the producer own the
contract that its success is completion. Regenerate producer prompts to gain
the clause; existing prompts keep the verifier.

### A blocked outcome is visible (`completion.blocked_when`)

A refresh that correctly decides not to publish — its source was unreachable,
the data failed a check — records that through the same completion tool, and
used to finish as a plain green success. In production 26% of refresh runs
ended that way, and a dashboard could stop updating for 10–14 days while
every run read green. The clause may say which recordings mean "blocked":

```text
EXECUTION REQUIREMENTS (JSON):
{"completion":{"any_succeeded":["mcp_pages_record_refresh_check","mcp_pages_update_page_data","mcp_pages_update_page_data_upload"],"blocked_when":{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked","failed","source_unreachable"],"detail_argument":"detail"}}}
```

- `tool` — required; must also be listed in `any_succeeded`, spelled the same
  way (the rule qualifies how the predicate completed the run). Same
  identifier rule as every other name; resolved against the roster like
  `any_succeeded`.
- `argument` — required; a top-level argument name of that tool's input
  (`^[A-Za-z_][A-Za-z0-9_]{0,63}$`). Nested fields are not matched.
- `in` — required; 1 to 20 values, each 1 to 100 printable characters,
  matched exactly against the argument's string value.
- `detail_argument` — optional; a top-level string argument of the same call
  whose text (whitespace collapsed, at most 300 characters) explains the
  outcome. Without it the detail is `<argument>=<value>` alone.

A malformed rule is refused at save time and at dispatch with a message that
names the field. Unknown keys inside it are ignored, and a Fleet build that
predates the rule ignores the whole `blocked_when` key, so producers can emit
it before every deployment has upgraded.

**At finish**, when the run completes through the predicate, the rule
matches when:

1. no `any_succeeded` tool *other than* `tool` succeeded during the run (a run
   that published is never blocked, whatever it recorded), and
2. the **last** successful execution of `tool` passed `argument` with a value
   listed in `in`.

A matching run still **finishes successfully** — retry, dead-letter and
recurrence behave exactly as for any success — but:

- the task carries run outcome `blocked` and the detail
  (`outcome=source_unreachable: <the call's detail text>`), persisted on the
  task row (`run_outcome`, `run_outcome_detail`, migration 074);
- the Operations Center shows an amber **Blocked** badge instead of the green
  success one, in the task list, the run history and the task detail, which
  also gains a *Blocked* row with the reason; `fleet sched task list` shows
  `success (blocked)`, and the task's `result` starts with `[blocked]`;
- the session log gains a `[completion_blocked] …` breadcrumb;
- the success notification's message says `Blocked: <detail>`. When a
  recurring lineage reaches **three consecutive** blocked occurrences, that
  run's notification is sent as a failure instead
  (`Blocked 3 runs in a row — the schedule keeps running, …`), so an owner who
  subscribes only to failures hears about a dashboard that stopped updating.
  It fires once per streak, at the third run. Nothing parks: the next run that
  publishes ends the streak.

Fleet assigns no meaning to the tool, the argument or the values beyond that
match; the producer owns which recordings mean blocked.

## Scope

This fixes conditional completion and provides early execution diagnostics for
copy/paste handoffs. MCP catalogs, credentials, tool contracts and customer
protocols remain in external config bundles; Fleet does not import or depend on
a producer application. This does not edit existing tasks or add a scheduling
UI/import API. Regenerate producer prompts to gain the prerequisite check.
Existing recurrence, retry and sandbox permissions are unchanged.
Existing dead-letter records are not rewritten, and deploying this fix does not
replay them. A dead-lettered *recurring* occurrence still queues the next run
(ADR-0070) unless two consecutive occurrences were dead-lettered; that is a
change to the schedule, not to what this verifier treats as success. Check
completed tool results before rerunning a failed task.
Provider recovery is described in [completed-step recovery](COMPLETED-STEP-RECOVERY.md).
It never blindly retries an external mutation. Customer source-grain migrations
remain outside the generic engine.
