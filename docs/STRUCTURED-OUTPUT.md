# Structured output contracts

A scheduled task that declares `output_schema` is asking Fleet for a required
machine-readable result, not a best-effort formatting hint. Fleet cannot mark
that task successful unless a locally validated `output_json` value is committed
with the success transition.

## Create a structured task

`output_schema` is a self-contained JSON Schema document object. The declared
result itself may be an object, array, scalar, or union. Internal `#/...` refs
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

- An active OpenRouter model receives the declared schema through strict
  `response_format: {type: "json_schema", ...}`. Fleet also sets OpenRouter's
  `require_parameters` routing constraint so it cannot silently choose an
  upstream that ignores the requested parameter. If the upstream rejects the
  strict payload outright (some upstreams enforce a strict-mode subset
  narrower than draft-07, e.g. every property required +
  `additionalProperties: false`), the phase downgrades ONCE to the forced-tool
  path below instead of failing a Fleet-valid schema deterministically.
- Other provider adapters receive exactly one forced `structured_output`
  function tool whose input schema is the declared schema. No ordinary task,
  MCP, or native tools are exposed in this phase.
- The terminal prompt is bounded by the same model-aware aggregate context
  reduction as every inner step (#793), so a long transcript cannot overflow
  the window exactly at the run's final — most expensive-to-lose — request.
- Each terminal request runs behind a bounded in-phase transient retry
  (backoff, no tools, so a retry can never repeat a side effect). Exhausted
  transient retries surface as the TRANSIENT infrastructure class — the
  standard re-queue policy — never as a model-format failure.
- Provider schema protocols require an object at the top level. Fleet passes a
  definitely object-root declaration directly; an array, scalar, union, or
  `$ref` root is placed under one deterministic `value` property and unwrapped
  before local validation and persistence, so the public result shape is still
  exactly the declared shape.
- If in-run failover selected a fallback model, the terminal phase uses that
  active model and its provider capability path; it does not return to the
  failed primary.

Fleet validates the candidate locally with the same schema compiler used at
enqueue. Invalid or missing output gets two dedicated correction attempts
(three terminal generations total). Each correction sees a concise validation
error and still has no ordinary tools, so it cannot repeat external side
effects. A provider refusal fails immediately. Fatal provider errors,
cancellation, ceiling exhaustion, missing output after the budget, and
permanently invalid output all end the run unsuccessfully with an actionable
diagnostic; transient provider errors are retried in-phase and, if
persistent, fail with the transient infrastructure class instead.

## Commit and retrieval contract

The runner defensively validates the handed-off terminal value again (the
exact bytes agentcore validated, redacted once at the driver boundary — never
a re-parse of the redacted display text, which could corrupt or fail an
already-valid contract; if redaction itself altered the JSON, the run fails
with an explicit diagnostic rather than committing corrupted output). Storage then
validates it a third time and writes `output_json` and `status=success` in one
lease-checked database transaction. Success notifications, email replies, and
creation of the next recurring occurrence happen only after that commit. A lost
lease cannot publish output, success, or success side effects.

`GET /tasks/{id}/output` returns the validated JSON directly with
`Content-Type: application/json`:

- `200`: validated output is available. Every successful task with
  `output_schema` has this response.
- `409`: the task is not terminal yet — poll again later.
- `410`: the task failed terminally; the declared output will never exist for
  this run. Stop polling.
- `404`: the task did not declare `output_schema` (or the task itself is not
  visible/found). A schema-validation failure is never reported as a missing
  optional resource.
- `500`: an old/corrupt row violates the invariant (success without
  output_json, or stored bytes that are not valid JSON). Stored output is NOT
  re-validated against today's schema complexity bounds on read — bounds apply
  at enqueue, so legacy successful output stays readable.

## Retry and dead-letter behavior

Fleet exposes two retry-policy classes:

- `structured_output_format`: refusal, missing output, fatal generation
  failure, or exhausted local validation/correction. Transient provider
  weather during terminal generation is NOT this class — it retries in-phase
  and then falls under the standard transient policy.
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
