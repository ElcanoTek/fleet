# ADR-0102: Structured, correlated logs and traces, including across the broker boundary

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** Fleet maintainers (#2052)
- **Relates to:** [ADR-0042](0042-child-side-mcp-scope-authorization.md)
  (the broker authorizes from its own bundle), the "credentials stay
  host-side" and "never in logs" invariants in `AGENTS.md`,
  [`docs/TELEMETRY.md`](../TELEMETRY.md)

## Context

When a turn went wrong, reconstructing it meant grepping ~880 unstructured
`log.Printf` lines and lining up timestamps. Only a handful of them carried a
conversation, turn or task id, each formatted differently. OpenTelemetry
covered only the HTTP middleware, so a slow turn could not be split into
model, tool, sandbox, broker and database time. Export was plaintext only.

Two parts of the fix touch invariants:

- The process log is one of the places credentials must never reach. Moving
  every line to structured attributes changes what the log redactor sees.
- Joining the MCP broker child's work to the caller's trace needs the caller's
  trace context on the request frame. That frame is the credential boundary
  (issue #167, ADR-0042), and its contract is that only public data crosses
  it.

## Decision

1. **One way to log.** Fleet code logs through `log/slog`: `logging.From(ctx)`
   where a context exists, `slog.*` otherwise, and `logging.Fatal` to exit. A
   `forbidigo` rule bans the standard `log` package's print, fatal and panic
   functions everywhere, tests included, with one documented file allowlisted
   (logging's own bridge test). A third-party library's `log` output is bridged
   into the same stream.
2. **Correlation is a context contract.** `request_id`, `user_id`, `conv_id`,
   `turn_id`, `task_id` and `run_id` are attached to the context where each
   becomes known: the HTTP middleware, the auth gates, turn start, task run,
   and broker call. `trace_id`/`span_id` always come from the live span.
3. **The log redactor reuses the existing rules.** Both log formats install
   one `ReplaceAttr` hook. It applies `internal/redact`'s canonical patterns
   and the same literal set the tool-output and broker scrubbers hold, plus the
   existing secret-key rule. No log-specific rule set exists.
4. **Spans carry ids and sizes, never content.** Span attributes never hold
   prompt or completion text, tool arguments or results, commands, paths,
   queries, or an error message. Errors are recorded by class. A trace backend
   is not redacted, so nothing that needs redacting goes there.
5. **The broker frame may carry public correlation data.** A request may carry
   `trace` (the W3C traceparent/tracestate) and `log` (the six correlation ids).
   Both are identifiers the parent already holds, never a credential, and
   nothing flows back over them. The child honours only the known `log` keys
   and drops anything else, so the parent cannot write arbitrary attributes
   into the credential owner's log. The child exports its spans with its own
   exporter, and the parent's sampling decision rides `traceparent`.
6. **Trace export is TLS by default.** Plaintext requires an explicit opt-in
   and a loopback endpoint, because spans carry user and task ids.

## Consequences

- Any fleet log line can be found by `turn_id`/`run_id`/`trace_id`. One trace
  shows a whole turn or run, including the brokered MCP calls.
- New unstructured logging fails lint.
- The credential boundary's data set grows by two id-only fields. It stays
  one-way for credentials, and the child-side authorization of ADR-0042 is
  untouched: correlation data authorizes nothing.
- Operators with a plaintext non-loopback Collector must move it to TLS. A
  loopback Collector needs `FLEET_OTEL_INSECURE=true`.
- Log message wording lost its interpolated values. Anything that grepped for a
  full formatted line must match the message plus attributes. The docs that
  quoted lines were updated.
