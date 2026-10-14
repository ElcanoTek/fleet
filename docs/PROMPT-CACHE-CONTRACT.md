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
   persona + protocols + the skill roster, assembled driver-side
   (`internal/agent`), plus the "MCP Tools (live registry)" section that
   `agentcore.Run` appends from the roster it built (`live_registry.go`):
   sorted names or sorted connector counts, no volatile tokens, so it is as
   stable across the turns of a conversation as the rest.
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
  tools (in the order given) → loader tools → MCP tools **in
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
  *Sanctioned exception:* the Runtime Date Context section
  (`runtimeDateContext`, `internal/agent/prompt.go`) embeds the current UTC
  date at **day** granularity — a deliberate compromise that costs exactly one
  cache miss per conversation per UTC-midnight rollover in exchange for
  correct "today" reasoning. Do not tighten it below day precision. The
  per-turn `runtime_today` restatement (`RuntimeDateTurnSuffix`,
  `internal/agentcore/datewindow.go`) lives in the **message history**, not this
  prefix, so a multi-day chat can be re-anchored every turn without an extra
  prefix miss (#1026) — see "History must be append-only" below.

### History must be append-only

Each request of a conversation must be a **strict prefix** of the next one:
nothing the model saw may be dropped, reordered or rewritten when the next turn
replays history. Anthropic's lookback and OpenAI's block cache tolerate a small
divergence near the tail; an implicit-only cache does not — Gemini 4 Argon
carries a cache over only when the next request appends to the previous one
(measured 2026-10-14, below).

So per-turn model-facing context belongs IN the turn, persisted, not in a
request-local tail:

- **Interactive chat** persists each turn's `runtime_today` note: the user
  entry stores the UTC day (`TextContent.RuntimeToday`, JSON
  `runtime_today`), `assembleTurnMessages` sends the note directly after the
  user message, and `replayHistory` regenerates the identical bytes in the same
  place on every later turn (`agentcore.RuntimeDateMessage`). The run sets
  `RunConfig.RuntimeDateInInput` so `agentcore.Run` does not add a second,
  request-local copy. A user row written before this has no day and replays
  with no note, exactly as it was sent: an existing chat takes one cache miss
  on its first turn after the upgrade and is append-only from then on.
- **One-shot runs** (scheduled tasks, sub-agent children) keep the
  request-local tail from `agentcore.Run`: the whole run is one append-only
  transcript, so the tail stays put for every step.
- **Guard:** `TestTurnRequestIsAPrefixOfTheNext` (`internal/agent`) builds two
  consecutive turns — across a UTC-midnight rollover — and fails if turn 1's
  messages are not a byte-exact prefix of turn 2's.
- **Still request-local:** `user_prompt_submit` hook context and the budget
  wind-down notice. A bundle that injects per-turn hook context re-opens this
  gap for its chats; the wind-down notice only appears near a run's budget
  ceiling.
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
to a provider honoring explicit breakpoints** — OpenRouter-form `anthropic/` and
`google/`, plus the provider-local `claude-`/`gemini-` forms a native provider
(#289) reports (see `explicitBreakpointPrefixes`); a leading `~` floating-alias
sigil is stripped first. Everything else relies on the upstream's implicit
caching and gets no markers. Placement (Anthropic allows 4 breakpoints per
request):

1. The **last system message** — anchors the stable system-prompt + tool prefix.
2. The **compaction summary** (`WithCompactionSummaryBreakpoint`, enabled in the
   shared loop) — a stable boundary between the cached head and the evolving
   tail after a compaction pass.
3. The **last two non-system messages** — a rolling recency window. **Not for
   Gemini** (`stablePrefixOnly`, below).

**Gemini gets the stable prefix only.** OpenRouter turns the *last* breakpoint
of a Gemini request into an explicit context cache — a billed write with a
5-minute TTL that does not refresh — and ignores the others. A breakpoint on
the moving tail therefore wrote a brand-new cache of the whole prompt on every
request and never read one back. Measured on `google/gemini-3.8-flash`
(2026-10-14, the live check below): the old placement billed ~2× the prompt
and a full write per request; a single system-prompt breakpoint writes once and
is read back in full on every later request — the same conversation went from
$0.025 to $0.0053. With a compaction summary present it is the last (and so the
used) breakpoint, which is stable until the next compaction. Gemini 4 Argon has
no explicit caching (implicit only, Google's docs) and was measured to ignore
`cache_control` entirely, so the rule neither helps nor hurts it.

Breakpoint placement is itself part of the contract: the last-system-message
breakpoint is what makes the prefix cacheable, so the system prompt must be the
leading, stable content.

## Live check (`TestPromptCacheLive`)

`internal/agentcore/prompt_cache_live_test.go` drives the real `Run` loop —
fleet's breakpoints, upstream pins, tool roster and request-local tail
messages — against OpenRouter, and reports per request the served provider,
prompt / cached / cache-write tokens and cost. Three turns, one tool call, a
~7K-token system prompt with a fresh nonce (so turn 1 is cold). It costs real
money, so it is opt-in:

```sh
FLEET_CACHE_LIVE=1 OPENROUTER_API_KEY=…   FLEET_CACHE_LIVE_MODELS=google/gemini-4-argon,anthropic/claude-opus-5.5   go test -tags fleet_host_executor -run TestPromptCacheLive -v ./internal/agentcore/
```

It fails when a request after the first reads nothing from cache, or when
fleet's recorded cached tokens disagree with the wire. `FLEET_CACHE_LIVE_TURN_GAP`
sets the pause between turns (default 5s); `FLEET_CACHE_LIVE_DUMP=<dir>` writes
each request body for inspection. Run it after any change to the prompt layout,
the breakpoints, or the upstream pins.

### Results, 2026-10-14

26 models — the latest from each major lab on OpenRouter, the Artificial
Analysis top list, and fleet's tiers — on the final code (Gemini stable-prefix
breakpoints + persisted `runtime_today`). "Hits" is how many of the three
requests after the cold first one read from cache.

| Lab | Model | Hits | Notes |
|---|---|---|---|
| Anthropic | `claude-opus-5.5`, `claude-sonnet-5.5`, `claude-fable-5.1`, `claude-haiku-5.5` | 3/3 each | ~11.6K of ~11.9K read; explicit breakpoints |
| OpenAI | `gpt-6-luna-pro` (default tier), `gpt-6-astra`, `gpt-6.1-sol`, `gpt-6.1-sol-pro` | 3/3 each | automatic |
| Google | `gemini-4-argon` (strong tier) | 3/3 | implicit only; ~6.7–6.9K of ~8.9K |
| Google | `gemini-3.8-flash` | 3/3 | one write, then the full ~8.5K read; see the Gemini placement note above |
| xAI | `grok-4.7` | 3/3 | |
| Qwen | `qwen3.8-max-0902`, `qwen3.8-max-prime` | 3/3 each | implicit; explicit breakpoints measured slightly dearer |
| Z.AI | `glm-5.3`, `glm-5.3-prime` | 3/3 each | |
| Moonshot | `kimi-k3` | 3/3 | |
| StepFun | `step-5-preview` | 3/3 | |
| DeepSeek | `deepseek-v4.1-flash` | 3/3 | |
| DeepSeek | `deepseek-v4-pro-0813` | 2/3 | served by a third-party host (Baidu), not DeepSeek's endpoint |
| MiniMax | `minimax-m3` | 2/3 | first repeat missed, then hit |
| Xiaomi | `mimo-v2.6-pro` | 2/3 | alternating hit/miss on one provider (GMICloud) |
| NVIDIA | `nemotron-3-ultra-550b-a55b` | 2/3 | alternating hit/miss on one provider (BaseTen) |
| Meta | `muse-spark-1.3` | 3/3 | small, intermittent hits |
| Meta | `muse-wonder` | 2/3 | ~116 tokens; intermittent upstream |
| Mistral | `mistral-large-4-0` | 0/3 | implicit cache warms only after several requests |
| Cohere | `command-a-plus` | 0/3 | no cache reads through OpenRouter at all |

Every model stayed on one provider for all its requests (pins or OpenRouter's
sticky routing), so the misses in the lower rows are the upstream's own cache
behaviour, not fleet's request shape: raw-HTTP runs with no breakpoints, a
system breakpoint, or fleet's placement gave the same pattern for Mistral,
Cohere and Meta. `amazon/nova-premier-v1` was dropped: Bedrock now returns 404
(end of life).

**Gemini 4 Argon across turns (fixed).** Before the change above, Argon missed
on every new user turn: the request-local `runtime_today` tail was not kept in
history, so turn N's request was never a prefix of turn N+1's. Replaying
fleet's exact wire bodies isolated it (with the tail, turn 2 missed; without
it, turn 2 read ~6.5K), and a six-turn chat cost ~60–75% more input. With the
note persisted in history, turn 2 reads ~6.7K like every later request.

**Known gap: cache writes are not recorded.** fantasy's OpenRouter usage
struct has no `cache_write_tokens` field, so `CacheCreationTokens` is 0 for
every OpenRouter model (the live check logs the difference instead of
failing). Default billing is unaffected — fleet uses OpenRouter's reported
`usage.cost` — but per-model pricing overrides that price cache writes, and the
`cache_creation_tokens` shown in session and task stats, under-count. The fix
belongs upstream in fantasy (parse `prompt_tokens_details.cache_write_tokens`
into `fantasy.Usage.CacheCreationTokens`).

## Kill switch & observability

- **Kill switch:** `FLEET_DISABLE_PROMPT_CACHE` (with `CHAT_`/`CUTLASS_` back-compat
  aliases) makes `promptCachingStep` a no-op.
- **Observability:** cached-token counts are tracked (`CachedTokens`) and surfaced
  in the admin stats / cost accounting, so an operator can watch the cache-hit
  ratio and notice a regression the guard test didn't catch (e.g. a system-prompt
  volatility that only manifests live).
