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
message, bounded and delimited as its own section. A task requirement to
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

A verifier that could not answer at all — a timeout, a provider failure, an
empty or unparseable reply — says nothing about the run. So it does not spend a
check (#1602): the call is retried once after a short pause. If the verifier
still cannot answer, what happens depends on the audit:

- **The audit cleared with no failed critical call** (no audit-gated tool whose
  last execution in the run failed). The run succeeds with a
  `completion_unverified_verifier_error` warning, recorded in the session log
  and at the head of the task's terminal message. It is not dead-lettered on
  the verifier's own outage. The phone-a-friend reviewer already failed open
  on its errors.
- **A failed critical call is on the record.** The outcome is genuinely in
  doubt, so the outage keeps the pre-#1602 semantics: it spends a check, and
  the third ends the run `ErrCompletionUnverified`.
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

Fleet checks this optional declaration at **dispatch**, before model execution.
A sealed task/global lockdown produces an actionable network error. After the
run's MCP scope and remote overlay are opened, missing advertised servers/tools
produce an actionable roster error. Tools may be native names, server tool names,
or Fleet's full `mcp_<server>_<tool>` names; full names avoid ambiguity. Model
resolution already happens before the run and remains mandatory.

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

| task | verifier demanded | what actually happened |
|---|---|---|
| `11b2880d` diageo-raptive-campaign | "publish_managed_data_update … update_page_data_upload" | run recorded `blocked`; page correctly untouched |
| `07e43224` sunbum-elc00176 | "update_page_data publish … expected_version=853" | no new coverage; `record_refresh_check(source_not_updated)` written |
| `4207d823` diageo-raptive-campaign | "build complete payload … update_page_data_upload" | blocked branch, recorded |
| `19c59abc` raptive-seller-view | — (the verifier call itself timed out) | dead-lettered after three checks |
| `00d8224c` central-garden | a wording nit in the report | data published |

The producer can declare that knowledge in the same requirements object:

```text
EXECUTION REQUIREMENTS (JSON):
{"mcp_servers":["pages"],"required_tools":["mcp_pages_get_page_data","mcp_pages_record_refresh_check","mcp_pages_update_page_data_upload"],"completion":{"any_succeeded":["mcp_pages_update_page_data","mcp_pages_update_page_data_upload","mcp_pages_record_refresh_check"]}}
```

**At dispatch**, `completion.any_succeeded` names are validated and resolved
the way `required_tools` names are:

- They may be native names, bare server tool names, or full
  `mcp_<server>_<tool>` names. The same identifier rule applies, and at most
  200 names are allowed.
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
