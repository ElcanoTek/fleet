# Conditional scheduled tasks

A scheduled task may explicitly require a no-update check instead of publishing
when its sources have no new coverage. The end-of-run verifier now receives
bounded structured fields from both tool arguments and tool results, separately,
in addition to call success. Previously it only saw names and success booleans,
so it could demand the write the task's selected branch forbade.

The verifier is instructed to verify the selected branch and all its prerequisites.
A recorded no-update claim is not proof that required sources were downloaded.
Failed calls, missing results and truncated evidence are not successful checks.
For long tasks, truncation retains both the opening identity and closing stop
rules. Only allowlisted scalar control fields and known envelope paths travel
to this extra model call: no raw report rows, code, URLs or free-text tool output.
The verifier remains a model-based check with the existing bounded invocation,
metering and fail-open error behavior; it is not a deterministic proof of data
reconciliation.

An explicit `confirm_audit(success=true, critical_actions=[])` now completes a
read-only/no-update audit without inventing a future mutation. It activates the
typed gate with no new commitments and therefore authorizes no new critical
calls. Existing outstanding commitments remain outstanding. Missing/null
successful-audit declarations still fail. Explicit terminal audit aborts retain
the failed run outcome and skip the driver reviewers instead of re-demanding
abandoned actions.

## Copyable execution prerequisites

Prompt producers may include one literal line followed by one JSON object:

```text
EXECUTION REQUIREMENTS (JSON):
{"mcp_servers":["reporting"],"required_tools":["mcp_reporting_download"],"network":true,"model_required":true,"mode":"managed_data"}
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
ignored for forward compatibility. Malformed/duplicate declarations fail closed.
Ordinary prompts with no marker keep their existing behavior.

## Scope

This fixes conditional completion and provides early execution diagnostics for
copy/paste handoffs; it does not create a Pages-specific scheduler, edit existing
tasks or add a scheduling UI/import API. Regenerate producer prompts to gain the
prerequisite check. Existing recurrence, retry and sandbox permissions are unchanged.
Provider errors remain governed by the existing typed status/SSE retry classifier;
a generic provider-error string without status is insufficient evidence to retry
an external mutation. Provider adapter diagnostics and customer source-grain
migrations are separate changes, not silently bundled into this fix.
