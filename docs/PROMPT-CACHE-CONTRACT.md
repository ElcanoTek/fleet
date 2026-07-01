# Prompt-cache prefix-stability contract (#507)

Prompt caching is fleet's largest input-cost lever: a cached prefix is billed at
roughly **10%** of the normal input rate. The provider (via OpenRouter) caches by
**exact byte prefix** — the cache keeps hitting only while the leading bytes of a
request are identical to a previous one. If a volatile value (a timestamp, a
non-deterministic map iteration order, a reordered tool list) lands anywhere in
that prefix, every subsequent byte differs and the cache silently misses,
restoring full input billing **with no build-time signal**.

This document is the contract for what must stay byte-stable, and points at the
guard test that fences it.

## The cacheable prefix

A request is, in order:

1. **System prompt** (one or more system messages) — the base instructions +
   persona + protocols + the MCP/skill roster. Assembled driver-side
   (`internal/agent`), stable across the turns of a conversation.
2. **Tool definitions** — the serialized tool roster (name, description,
   parameters schema, required list) the model may call. Assembled in
   `internal/agentcore` (`buildFantasyTools`).
3. **Message history** — the evolving tail (not part of the stable prefix; the
   rolling recency breakpoints in `cache.go` handle it separately).

Items **1 and 2 are the cacheable prefix.** They must be byte-identical from one
turn (and one process) to the next for the same conversation/config.

## What MUST stay byte-stable

### Tool definitions (agentcore-owned — guarded)

- **Tool order.** `buildFantasyTools` appends to a slice in a fixed order: native
  tools (in the order given) → loader tools → pre-gated tools → MCP tools **in
  catalog order** → optional `confirm_audit`. The MCP catalog must be a stable,
  ordered slice — never sourced from a Go `map` whose iteration order is random.
- **Schema serialization.** A tool's `parameters` is a `map[string]any`.
  `encoding/json` **sorts map keys** on marshal, so map-valued schemas serialize
  deterministically — *as long as the value never goes through a non-sorting
  serializer* (e.g. `fmt.Sprintf("%v", m)`, a hand-rolled encoder, or a
  `text/template` range over a map). Keep schema serialization on `encoding/json`.
- **`required` order.** `mcpTool.Info()` preserves the source order of the
  schema's `required` array. That source (the MCP server's declared schema, or a
  bundle `http_tools` entry) must itself be stable — do not build it by ranging a
  map.
- **No volatile tokens.** No timestamps, random ids, hostnames, or per-process
  values in a tool name, description, or schema.

**Guard:** `internal/agentcore/prefix_stability_test.go` fences this half:
- `TestToolPrefixDeterministic` builds the same roster 64× and asserts
  byte-identical serialization (catches any non-deterministic ordering).
- `TestToolPrefixOrderStable` locks the native-then-MCP-catalog-order roster.
- `TestToolPrefixGolden` locks the exact serialization format against a checked-in
  golden — a format change (field order, added field, whitespace) fails it and
  forces a conscious update + a re-check of this contract.

### System prompt (driver-owned — contract, not yet golden-guarded)

The drivers (`internal/agent` interactive + scheduled) must keep the system
prompt stable within a conversation:

- **No volatile tokens near the prefix.** Do not embed `time.Now()`, a random
  request id, or a per-turn counter in the system prompt. (Per-turn dynamic
  context belongs in the message tail, not the cached system prefix.)
- **Deterministic roster.** The MCP/skill/persona roster injected into the prompt
  must be built from a stable, ordered source (sort before joining), never a raw
  map range.
- **Stable file contents.** The base prompt files (`system_prompts/*.md`) and
  persona/protocol files are static bundle content; changing them is a
  deliberate, cache-busting act (and correct — the prefix genuinely changed).

A byte-golden for the assembled system prompt is a reasonable follow-on; today
the contract is enforced by review + the no-volatile-tokens rule above.

## Where the breakpoints are placed

`internal/agentcore/cache.go` (`promptCachingStep`) installs explicit
`cache_control: {type: "ephemeral"}` breakpoints, but **only for slugs that route
to a provider honoring explicit breakpoints** — `anthropic/` and `google/`
(see `explicitBreakpointPrefixes`); a leading `~` floating-alias sigil is stripped
first. Everything else relies on the upstream's implicit caching and gets no
markers. Placement (Anthropic allows 4 breakpoints per request):

1. The **last system message** — anchors the stable system-prompt + tool prefix.
2. (optional) The **compaction summary**, when `WithCompactionSummaryBreakpoint`
   is set — a stable boundary between the cached head and the evolving tail.
3. The **last two non-system messages** — a rolling recency window.

Breakpoint placement is itself part of the contract: the last-system-message
breakpoint is what makes the prefix cacheable, so the system prompt must be the
leading, stable content.

## Kill switch & observability

- **Kill switch:** `FLEET_DISABLE_PROMPT_CACHE` (with `CHAT_`/`CUTLASS_` back-compat
  aliases) makes `promptCachingStep` a no-op.
- **Observability:** cached-token counts are tracked (`CachedTokens`) and surfaced
  in the admin stats / cost accounting, so an operator can watch the cache-hit
  ratio and notice a regression the guard test didn't catch (e.g. a system-prompt
  volatility that only manifests live).
