# Structured output contracts

A scheduled task that declares `output_schema` is asking Fleet for a required
machine-readable result, not a best-effort formatting hint. Fleet cannot mark
that task successful unless a locally validated `output_json` value is committed
with the success transition.

## Create a structured task

`output_schema` is a self-contained JSON Schema object. Internal `#/...` refs
are supported; external refs are rejected because schema compilation runs in the
host process and must never read a task-selected URL or file.

```json
{
  "prompt": "Summarize the latest deployment result.",
  "output_schema": {
    "type": "object",
    "properties": {
      "healthy": {"type": "boolean"},
      "summary": {"type": "string"}
    },
    "required": ["healthy", "summary"],
    "additionalProperties": false
  }
}
```

Every enqueue seam rejects a schema before persistence when it is malformed or
exceeds any protocol limit:

| Limit | Maximum |
| --- | ---: |
| Encoded schema size | 65,536 bytes |
| Root-to-node JSON depth | 32 levels |
| JSON values (objects, arrays, and scalar nodes) | 2,048 |

The compact schema appears once in the run's bounded prompt instruction and once
in the terminal provider configuration. Each logical copy starts from at most
65,536 bytes; normal JSON wire escaping is still linearly bounded. Correction
messages add only a 1,024-byte validation diagnostic, not another schema copy.

## Terminal generation

The task's ordinary work still runs through the single governed
`agentcore.Run`: mandatory sandbox, audit and finish gates, host-side
credentials, tool policy, and cost/token ceilings all remain in force. Only
after the ordinary tools have finished and the policy permits completion does
that same run enter its terminal formatting phase over the completed
transcript.

- An active OpenRouter model receives the full raw schema through strict
  `response_format: {type: "json_schema", ...}`. Fleet also sets OpenRouter's
  `require_parameters` routing constraint so it cannot silently choose an
  upstream that ignores the requested parameter.
- Other provider adapters receive exactly one forced `structured_output`
  function tool whose input schema is the declared schema. No ordinary task,
  MCP, or native tools are exposed in this phase.
- If in-run failover selected a fallback model, the terminal phase uses that
  active model and its provider capability path; it does not return to the
  failed primary.

Fleet validates the candidate locally with the same schema compiler used at
enqueue. Invalid or missing output gets two dedicated correction attempts
(three terminal generations total). Each correction sees a concise validation
error and still has no ordinary tools, so it cannot repeat external side
effects. A provider refusal fails immediately. Provider errors, cancellation,
ceiling exhaustion, missing output after the budget, and permanently invalid
output all end the run unsuccessfully with an actionable diagnostic.

## Commit and retrieval contract

The runner defensively validates the final assistant value again. Storage then
validates it a third time and writes `output_json` and `status=success` in one
lease-checked database transaction. Success notifications, replies, downstream
triggers, and creation of the next recurring occurrence happen only after that
commit. A lost lease cannot publish output, success, or success side effects.

`GET /tasks/{id}/output` returns the compact validated JSON directly with
`Content-Type: application/json`:

- `200`: validated output is available. Every successful task with
  `output_schema` has this response.
- `409`: the task is not terminal yet, the structured task ended unsuccessfully,
  or an old/corrupt database row says success without schema-valid output and
  violates the current invariant.
- `404`: the task did not declare `output_schema` (or the task itself is not
  visible/found). A schema-validation failure is never reported as a missing
  optional resource.

## Retry and dead-letter behavior

Fleet exposes two retry-policy classes:

- `structured_output_format`: refusal, missing output, generation failure, or
  exhausted local validation/correction.
- `structured_output_persistence`: validated JSON could not be committed under
  the held lease.

Both are non-retryable by default and therefore go to the dead-letter path
instead of silently succeeding. An operator may explicitly list either class in
`retry_policy.retry_on` and provide `max_retries`. That opt-in retries the whole
scheduled task through the normal claim path, not just terminal formatting, so
it carries the same external-side-effect considerations as every whole-task
retry. The in-run two-attempt correction loop never re-runs ordinary tools.

Free-form tasks (no `output_schema`) retain their existing transcript and
success behavior; this contract does not force JSON onto them.
