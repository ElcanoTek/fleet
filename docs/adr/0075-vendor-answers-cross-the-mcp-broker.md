# ADR-0075: A hosted vendor's own answer to a failed tool call crosses the MCP broker, bounded and scrubbed

- **Status:** Accepted (amends the error clause of ADR-0040)
- **Date:** 2026-09-29
- **Deciders:** fleet maintainers

## Context

ADR-0040 put per-user hosted MCP connections in a credential-owning child
process and fixed the rule for what a failed operation may say to the
parent: nothing. Every call, discovery, scope-open, scope-close and reload
error crossed the pipe as a stable, value-free class such as
`credential-owner call failed`, because an operational error can embed
connector stderr, resolved URLs, headers or provider detail — the very
things the boundary exists to keep out of the model context and the logs.

The #1006 verification found the cost of applying that rule to a vendor's
own answer. Stripe refused a tool call with HTTP 422 and the message
"Missing required parameter: stripe_context"; the model saw only the masked
class, read it as a dead credential and told the user to reconnect a working
connection (finding F8). A vendor's reply to a request is not operational
detail from inside the deployment: it is the same class of text as a tool
result, which already crosses the boundary unmasked.

## Decision

For a call routed through a **hosted** (per-user remote) scope, three
classes of failure now cross the pipe, each rendered by
`mcpbroker.describeCallError` and bounded and scrubbed by
`sanitizeVendorText`:

- an HTTP 4xx other than 401 — the status and the vendor's first line;
- a JSON-RPC error object — its code and message;
- an HTTP 401 — the status only, worded as a credential rejection, because
  the body of a refused request is the one answer that can quote the request
  back.

Everything else keeps the ADR-0040 rule: a 5xx, a transport or context
failure, an unknown error type, and **every** error of a bundle server
(stdio or bundle HTTP), whose tracebacks and upstream URLs are deployment
detail. The child marks a hosted server's error with
`mcpbroker.HostedCallError`; the broker passes nothing that lacks the mark.

Scrubbing, in order: credential carriers masked by shape (a query parameter
named like a key or token, a `Bearer`/`Basic` scheme value), both
process-wide literal redactors — the child's, which holds every hosted
credential it acquired, now registered in every wire spelling a vendor can
echo (query-escaped, path-escaped, JSON-escaped) — control characters
stripped, whitespace collapsed, redacted once more, and cut to 240 bytes on
a rune boundary. The full detail is still logged host-side first, and the
log line says what the peer was told.

## Consequences

- The model can correct a call the vendor refused over an argument, a
  permission or a rate limit, instead of sending the user to reconnect.
- The boundary is narrower than ADR-0040 stated and this document is the
  record of exactly how: a reader of `docs/MCP-BROKER-SCOPES.md` must not
  assume broker errors are value-free.
- A vendor that echoes a request credential in a 4xx body in a spelling
  neither the shape masks nor the registered spellings cover would leak it
  into the model context. The shape masks and the spelling registration are
  the mitigations; the residual risk is a key under 8 bytes echoed in a form
  the shape masks do not match, which the literal floor (`internal/redact`)
  cannot cover.
- Bundle servers gain nothing here on purpose; their operators read the
  host log.
