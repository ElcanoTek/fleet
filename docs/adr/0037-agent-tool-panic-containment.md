# ADR-0037: Contain panics at the AgentTool dispatch boundary

Status: accepted

## Context

Fleet deliberately runs interactive chat, scheduled tasks, and its worker pool
in one process. Fantasy v0.35 streams tool calls into a coordinator goroutine
and launches calls marked parallel in additional goroutines. Its
`executeSingleTool` invokes `AgentTool.Run` without panic recovery.

Go recovery is goroutine-local. Recovery around an HTTP handler or scheduled
worker therefore cannot intercept a panic in Fantasy's coordinator or parallel
worker. One buggy native tool, MCP adapter, policy hook, output guardrail, or
observer could terminate every run hosted by the process.

Recovering inside individual tools is insufficient: Fleet has native, loader,
direct MCP, and deferred MCP registration routes, and a new route
could silently omit a local wrapper. Recovering before result accounting could
also break the provider transcript's required tool-call/result pairing.

## Decision

Fleet owns one outer `AgentTool` dispatch wrapper applied to the final roster.
It recovers in the goroutine calling the inner tool, emits structured telemetry,
and returns one nil-Go-error, `IsError` response carrying only an opaque incident
reference. Deferred logical MCP tools are wrapped before registry insertion in
addition to the advertised disclosure bridges, preserving logical attribution
without double-reporting.

Fleet removes the unused arbitrary `PreGatedTools` seam. A black-box tool that
invokes `Policy.RecordToolResult` internally cannot tell the outer recovery
whether it recorded before panicking, so exactly-once repair is unprovable.
Externally supplied tools enter through native/loader routes and Fleet owns
their policy lifecycle; the built-in MCP adapter is the only self-accounting
route and advances the invocation-local marker itself.

Fleet tracks the active policy/dispatch/output phase in invocation-local
context. Panic diagnostics include a value-free class, the phase, tool/call
identity, mode, and available task/conversation identity; no tool arguments or
results are attached. Raw recovered values and stacks are discarded before
logging, Sentry, or `panic_events`, and the model-visible response contains only
the opaque incident reference.

No tool-supplied Go error object crosses into Fantasy. Fleet evaluates its
`Error()` method under the outer recovery boundary, passes the resulting text
through the same secret/PII/guardrail/size pipeline as ordinary output, and
returns a plain Fleet-owned error. An attributed result-flattening boundary
preserves an opaque paired result if a provider/validation error still has a
panicking `Error()` implementation. The stream sink keeps a deterministic
secret-only backstop for Fantasy-generated errors without re-invoking the
configurable output components that may have caused the incident.

When `BeforeToolCall`, execution, or output screening panics before normal
result accounting, the outer boundary records exactly one failed
`Policy.RecordToolResult` for the logical tool using the safe incident response.
Deferred MCP records the logical
`mcp_<server>_<tool>` failure, not merely its disclosure bridge. The boundary
marks an accounting attempt before invoking it, so a panic in
`RecordToolResult` is contained but never retried.

A recovered panic is conservatively a possibly committed execution. The tool
call remains in the stream sink, its paired error result remains in the
transcript, and ADR-0035's tool-event gate prohibits an in-round re-drive that
could repeat a side effect.

Observers are wrapped once at `agentcore.Run`. Delivery is serialized and
disabled after the first panic. Fantasy is allowed to settle every in-flight
tool before the stored incident becomes an ordinary run error. Policy
`CanFinish` panics, which have no tool result to pair, likewise become ordinary
opaque run errors.

This adds a process-safety invariant: no synchronous panic from a Fleet-owned
tool dispatch, policy/output phase, or observer callback may escape the
governed run boundary.

## Consequences

A single tool or callback defect fails one call or run rather than the Fleet
process. Parallel siblings settle independently, and persisted transcripts keep
valid call/result pairing. Operators can correlate the opaque user/model
reference with logs, Sentry, and `panic_events`.

Removing raw panic values and stacks sacrifices some post-incident debugging
detail, but prevents credentials embedded in a panic or stack-local data from
becoming durable telemetry. Phase and identity attribution plus the opaque
incident reference remain available for correlation.

The boundary cannot contain a panic in a goroutine a tool launches and abandons;
such goroutines remain subject to `internal/safe`'s entry-point rule. Remote
process failures remain ordinary transport errors. Serialization means a slow
observer already delayed its own callback path and now also orders concurrent
observer deliveries; tool execution itself remains parallel.
