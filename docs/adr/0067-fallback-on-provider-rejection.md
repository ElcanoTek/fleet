# ADR-0067: Promote the fallback model on a per-request provider rejection

Status: accepted; narrows the promotion clause of ADR-0033 and keeps the
suppression rules of ADR-0035 / ADR-0065.

## Context

ADR-0033 promotes a configured fallback only for retryable provider or
transport failure; every non-retryable status is terminal. Fleet routes models
through a gateway (OpenRouter) that relays an upstream provider's own 4xx as
the opaque `Provider returned error`, with the upstream cause carried only in
`error.metadata`. On 2026-09-16 a scheduled run completed 45 tool steps on
`google/gemini-3.8-flash`, then Google rejected the 46th request with a 400;
the run dead-lettered as `bad request: Provider returned error` and the
configured `fallback_model` was never consulted. Three earlier runs on the
same model died the same way, and a re-run succeeded. The request that one
provider rejects is frequently accepted by another model, and nothing in the
dead-letter reason said which provider rejected it or why.

## Decision

A non-retryable provider error with an explicit 4xx status is a
*provider rejection* unless it is a credential failure (401, or the adapter's
`AuthError` flag) or a billing failure (402), which bind every model behind
the same key. A rejection promotes the configured fallback model exactly once,
and only from a safe point: no tool event may have occurred since the attempt
mark, which ADR-0065 advances to the last completed-step checkpoint. The
fallback therefore re-drives with the completed tool results in its input and
never replays an executed call. When no fallback exists, the fallback equals
the active model, or a tool ran without a checkpoint, the rejection remains
terminal exactly as before; it is never reclassified as transient.

Terminal provider errors and the exported `[provider-failure]` log note name
the relayed upstream cause: the gateway's `provider_name`, `error_type`,
`provider_code`, and a redacted, bounded `raw` message. Only those named
metadata fields are read; response headers and the rest of the body are still
never logged.

## Consequences

A configured `fallback_model` now covers the failure mode it was configured
for. Deterministic bad requests cost at most one extra fallback attempt before
failing with the same class as today. Operators can distinguish an upstream
provider rejection from a gateway or fleet fault from the dead-letter reason
alone. `max_retries`, network permissions, credentials and connector contracts
are unchanged. See [implementation and scope](../COMPLETED-STEP-RECOVERY.md).
