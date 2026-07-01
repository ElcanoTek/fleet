# BM25 progressive tool disclosure

A large MCP catalog used to be a hard error: over 128 tools blew the provider's
per-request ceiling, and every tool's schema was billed on every turn. #506
removes the ceiling and cuts per-turn tokens by deferring most tools behind
three bridge tools backed by an in-process BM25 keyword index — no embeddings,
no vector DB, no network. (ADR-0026.)

## How it works

`buildFantasyTools` (the ONE builder both interactive and scheduled runs feed)
counts the roster it would register. When that exceeds the disclosure threshold
(`FLEET_TOOL_DISCLOSURE_THRESHOLD`, default 128 = the provider ceiling), it:

- keeps **core tools** directly registered — native (bash/python/files/…),
  loader, pre-gated, and confirm_audit are NEVER deferred;
- hides the **MCP tools** (those that already passed the opt-in + allowlist
  gates) behind three bridges:
  - `tool_search {query}` — BM25 keyword search over `{name, description}` →
    top-K names + one-line descriptions;
  - `tool_describe {name}` — the tool's full description + JSON parameter schema;
  - `tool_call {name, arguments}` — dispatches to the real tool's `Run`.

A deferred `tool_call` routes through the **same `*mcpTool` wrapper** a direct
call would, so the MCP broker + per-task credential allowlist (#184), the policy
gate (BeforeToolCall/RecordToolResult), output redaction + ceiling, and audit
all apply identically — a deferred tool is first-class, just not always
advertised.

Below the threshold nothing changes: every tool registers directly, byte-for-
byte as before (so the #507 prompt-cache prefix stays intact for small
catalogs).

## BM25 index (`internal/tools/bm25_index.go`)

Textbook BM25 (k1=1.5, b=0.75) over tokenized tool metadata. The tokenizer
splits `snake_case` and `camelCase` so "send email" matches both `send_email`
and `sendEmail`. It's pure, deterministic, and dependency-free — the same index
is reusable for connector recommendation (#512).

## Honest scope

- Deferral triggers on COUNT, not on token budget — a large-but-under-threshold
  catalog still registers directly (raise/lower `FLEET_TOOL_DISCLOSURE_THRESHOLD`
  to tune; e.g. set it below your typical roster to always defer and shave
  tokens).
- BM25 is keyword ranking; it won't match a purely semantic query with zero
  lexical overlap. For tool names/descriptions that's rarely an issue, and the
  model can re-query with different keywords.
- Name collisions across servers resolve last-write-wins in the deferred
  registry, matching direct registration.
