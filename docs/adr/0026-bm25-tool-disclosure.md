# ADR-0026: Progressive tool disclosure via an in-process BM25 index

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Over 128 tools is a hard provider error, and interactive turns already run near
that ceiling once per-user remote MCP servers load (#443/#449); every tool
schema is also billed every turn. A large connector catalog is table stakes,
but it must still route through host-side policy + credential brokering.

## Decision

When the roster would exceed a threshold, defer the MCP tools behind
`tool_search → tool_describe → tool_call`, backed by an in-process BM25 index:

1. **BM25, not embeddings** (`internal/tools/bm25_index.go`): keyword ranking is
   plenty for tool names/descriptions, keeps the feature self-hosted,
   deterministic, and free (no vector DB, no external service — fleet's
   single-box model), and the index is reusable for #512.
2. **Core tools never defer.** Only MCP tools (post opt-in + allowlist) hide;
   native/loader/pre-gated/confirm_audit stay directly callable.
3. **Deferred calls stay first-class.** `tool_call` dispatches to the same
   `*mcpTool.Run`, so the broker, credential allowlist, policy gate, redaction,
   output ceiling, and audit apply identically — deferral changes visibility,
   not governance.
4. **One builder, both modes.** The branch lives in `buildFantasyTools`, which
   interactive and scheduled runs share, so a scheduled task never silently
   gets a reduced roster.
5. **Small catalogs unchanged.** Deferral triggers only above
   `FLEET_TOOL_DISCLOSURE_THRESHOLD` (default 128), so the #507 prompt-cache
   prefix and existing behavior are byte-for-byte preserved for normal rosters.

## Alternatives rejected

- **Embeddings / a vector DB** — a network dependency and operational weight the
  single-box deployment doesn't need; keyword search suffices for tool metadata.
- **Silently truncating the roster at 128** — drops capability with no way to
  reach it; disclosure keeps every tool callable.
- **A separate builder for large catalogs** — would risk interactive/scheduled
  drift; the shared `buildFantasyTools` branch keeps one path.
