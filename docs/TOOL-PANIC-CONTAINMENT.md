# Agent tool panic containment

Fleet is one long-lived process: interactive turns, scheduled work, sandboxes,
and worker coordination share its fate. Fantasy executes streamed tools in its
own coordinator goroutine and may execute tools marked parallel in additional
goroutines. Go panic recovery is goroutine-local, so HTTP or worker-level
recovery cannot contain a panic raised there.

## Shipped boundary

Every Fleet-owned `fantasy.AgentTool` roster now crosses one final
`panicContainedTool` boundary immediately before registration. This includes
native tools, scheduled loader tools, direct MCP adapters,
progressive-disclosure bridge tools, and `confirm_audit`. Deferred MCP tools
also cross the same boundary before entering the hidden registry, so an
incident is attributed to the logical `mcp_<server>_<tool>` call rather than
only to the `tool_call` bridge. The interactive leaked-call finalizer reuses
this final governed roster; it no longer registers raw driver tools on its own.
The unused arbitrary `PreGatedTools` registration seam was removed: because
those black-box tools owned policy hooks internally, an outer recovery could
not distinguish "recorded, then panicked" from "panicked before recording" and
could not guarantee exactly-once accounting. Externally supplied tools now use
the native/loader routes whose policy lifecycle Fleet owns.

Recovery runs in the exact Fantasy goroutine that invokes `AgentTool.Run`. A
panic becomes a single `IsError` tool response with the original call ID. The
model sees only:

```text
Tool execution failed unexpectedly. Reference: inc_<opaque-id>. The call may
have committed side effects; verify state before retrying.
```

The raw panic and stack are discarded at the recovery telemetry seam. Fleet
records the same incident ID in structured logs, `safe.PanicStats`, Sentry, and
the append-only `panic_events` ledger (migration 040), together with a
value-free panic class, boundary phase, tool name, call ID, run mode, and the
task or conversation ID when supplied. New rows use the existing
`panic_events.message` column for that class and leave `stack` empty; migration
040 also scrubs the old diagnostic values from preexisting rows. Tool input,
tool output, prompts, credentials, recovered values, and stacks are never
attached.

Fleet advances an invocation-local phase marker before `BeforeToolCall`, tool
dispatch, output secret/PII redaction, output guardrail screening, and
`RecordToolResult`. The one outer recovery point therefore retains normal
result pairing while still identifying which phase panicked. `CanFinish`
panics have no tool call to pair with, so they become an ordinary opaque run
error after telemetry is recorded.

Tool-supplied Go errors are flattened, screened, bounded, and replaced with a
plain Fleet-owned error before Fantasy receives them. This prevents a secret in
`Error()` from bypassing output governance and lets the outer boundary contain
an `Error()` method that itself panics. Result flattening has a second attributed
recovery seam so even a provider/validation error with a hostile `Error()` keeps
one opaque call/result pair. The stream sink reapplies only the deterministic
secret scrubber as a final defense for Fantasy-generated validation errors; it
does not re-run configurable PII/guardrail components after a contained panic.

Normal policy accounting runs after execution and output screening. If a panic
in `BeforeToolCall`, execution, or output screening skips that path, the
recovery boundary records one failed
`Policy.RecordToolResult` for the logical tool using only the safe incident
response. This includes deferred MCP, where the logical
`mcp_<server>_<tool>` failure is recorded exactly once in addition to the
bridge's own `tool_call` bookkeeping. A panic in the accounting hook itself is
not retried and is contained as a separate operator incident.

## Side-effect and concurrency semantics

A recovered call is marked `possibly_committed` in client metadata because a
tool or result-recording panic can occur after an external mutation. More
importantly, Fantasy emits `OnToolCall` before dispatch; Fleet's stream sink
records that call before the recovery response. ADR-0035 therefore suppresses
same-round provider recovery after a panicked call just as it does after any
other potentially side-effecting execution. Fleet never re-drives the round
merely because the tool panicked.

Parallel calls are independent. One tool's panic produces one error result;
Fantasy waits for every sibling goroutine, and successful siblings keep their
own results. The transcript stores one call/result pair per ID regardless of
completion order.

The run's `Observer` is wrapped once and serialized. On its first panic, Fleet
records an attributed incident and permanently disables delivery to that
observer. Fantasy callbacks return normally so all in-flight tools settle;
after the stream returns, `agentcore.Run` converts the stored incident into an
ordinary run error. Observer panic values and stacks are discarded before
telemetry just like tool panic diagnostics.

## Scope and limits

- This contains synchronous panics raised while Fleet calls a tool, its policy
  and output phases, or an observer callback. A tool that starts its own
  unsupervised goroutine must still use `safe.Go`/`safe.Recover`; no caller can
  recover a panic in an unrelated goroutine.
- A remote MCP process crash is a transport failure, not a Go panic, and keeps
  the existing broker error path.
- Containment does not relax execution policy. Tools still execute only through
  the mandatory sandbox or host-side credential broker, and both modes still
  use the single `agentcore.Run` core.
- Operator telemetry intentionally retains only an opaque incident ID,
  value-free panic class, and non-content attribution. It never retains the
  recovered value or stack, and those fields never enter the model transcript
  or SSE payload.

The focused suite exercises sequential and parallel Fantasy streams, valid
persistable call/result pairing, native/direct-MCP/deferred-MCP/loader
registration, all policy/output phases, observer failure, incident attribution,
failed logical-tool accounting, JSON persistence/replay, secret-free telemetry,
and race-safe sibling settlement.
