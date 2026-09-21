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

The verifier remains a model-based check, not deterministic proof of a business
workflow. It runs at most three times: the initial check and two repair reviews.
Missing actions or a malformed/failed verifier response keep completion blocked;
the third unsuccessful check returns `ErrCompletionUnverified` directly through
the governed core, preserving partial work, usage and the completed-action count.
It does not ask the model to abort or run more tools: an audit abort may be
refused after all committed writes succeeded. A verifier failure remains a
terminal failure under the existing retry policy, never a successful completion.
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
