# ADR-0033: Cross-provider failover before stream commitment

Status: accepted

## Context

ADR-0017 introduced native provider routing but deferred cross-provider
fallback. A provider-specific outage can still fail work even when another
configured backend serves the same model.

## Decision

The bundle may declare `fallback_providers`, an ordered list of configured
provider names. For an implicitly routed model, the resolver prepares the next
eligible backend serving that exact provider-local slug. The existing resilience
loop promotes it only for retryable provider/transport failure. Explicit
`provider/model` routing remains pinned and receives no implicit fallback.

Fallback is suppressed after any text, reasoning, tool call, or tool result has
become observable. Fleet never splices streams or repeats a potentially
side-effecting tool sequence on another provider. The original run context,
deadline, accounting, ceilings, and circuit breaker remain in force.

## Consequences

Availability improves for pre-stream provider failures. Failures after stream
commit remain terminal by design. `fallback_model` continues to mean a deliberate
model-level alternative and takes precedence for scheduled work; provider
fallback preserves model identity.

