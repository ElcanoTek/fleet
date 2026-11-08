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

Those three feed `buildFantasyTools` a fixed catalog, so they cannot see where
the catalog comes from. Its source is `mcp.Client.GetAllTools`, which lists
servers in sorted-name order and each server's tools in its own `tools/list`
order; `TestGetAllToolsOrderIsStable` (`internal/mcp`) fences that. Every chat
turn binds a fresh client, so before that sort the per-turn catalog followed Go
map order and changed between turns.

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
(measured 2026-10-14, below) — and Gemini 3.8 Flash relies on that implicit
cache once its history outgrows the marked prefix (below).

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

`internal/agentcore/cache.go` (`PromptCachingStep`) installs explicit
`cache_control: {type: "ephemeral"}` breakpoints, but **only for slugs that route
to a provider honoring explicit breakpoints** — OpenRouter-form `anthropic/` and
`google/`, plus the provider-local `claude-`/`gemini-` forms a native provider
(#289) reports (see `explicitBreakpointPrefixes`), and any slug containing
`anthropic.claude-`, which every Bedrock Claude id does, inference profiles
included (`bedrockClaudeMarker`; an ARN matches only when it contains such an
id, so an application-inference-profile or provisioned-throughput ARN does
not); a leading `~` floating-alias sigil is stripped first. Everything else relies on the
upstream's implicit caching and gets no markers. Placement (Anthropic allows 4 breakpoints per
request):

1. The **last system message** — anchors the stable system-prompt + tool prefix.
2. The **compaction summary** (`WithCompactionSummaryBreakpoint`, enabled in the
   shared loop) — a stable boundary between the cached head and the evolving
   tail after a compaction pass. When no compaction summary is present yet,
   Anthropic's spare 4th breakpoint is placed on the **most recent unmarked
   `runtime_today` message** before the rolling tail: during a multi-step tool
   turn this keeps the turn-start prefix refreshed across every tool step, and
   on the next user turn (after `replayHistory` drops extended-thinking blocks
   from the prior turn's tool calls) it gives Anthropic an explicit breakpoint
   directly on that turn-start boundary even when the prior turn made >10 tool
   calls (>20 content blocks, past Anthropic's 20-block lookback window).
3. The **last two non-system messages** — a rolling recency window. **Not for
   Gemini** (`stablePrefixOnly`, below), which also drops 1 and 2 once the
   rest of the prompt outgrows them.

**OpenRouter wire normalization (`openRouterCacheControlTransport`).**
`fantasy`'s OpenRouter hook serializes single-text-part `user` and `assistant`
messages as plain string `"content": "..."` with top-level `"cache_control"` on
the message object. OpenRouter's OpenAI-to-Anthropic/Gemini translator honors
top-level `"cache_control"` on `system` and `tool` messages, but ignores it on
`user` and `assistant` messages unless `"content"` is a content-part array whose
text block carries `"cache_control"`. `openRouterCacheControlTransport`
(`internal/agentcore/provider.go`) normalizes non-empty string `"content"` on
`user` and `assistant` messages for explicit-breakpoint families into a
one-element `[{"type": "text", "text": "..."}]` array and moves any top-level
`"cache_control"` onto the last content block (when `tool_calls` is empty), so
Step 0 and every rolling `user`/`assistant` breakpoint write and read the full
conversation history cache.

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

**…until the rest of the prompt outgrows that prefix.** A Gemini request that
uses an explicit cache gets nothing past it from Google's implicit cache, so
with the marker on the system prompt a long run's history was never cached.
Two measurements on `google/gemini-3.8-flash` through OpenRouter's Google
endpoint (2026-11-01):

- A scheduled run captured through a logging proxy (the store-KPI task in
  `docs/SCHEDULED-COMPACTION-CHECKPOINTS.md`): its requests were strictly
  append-only between pauses, yet each read back only its marked prefix,
  9,068 tokens (the system prompt and tools) and after the first pause 10,511
  (with the compaction summary), while the prompt grew past 100K.
- A raw-HTTP probe outside fleet: an ~11K-token system prompt, then six
  requests three seconds apart, each adding a one-word assistant reply and a
  user turn of two ~25 KB JSON reports:

| Request | Prompt | Read from cache, system marker | Read from cache, no marker |
|---|---|---|---|
| 1 | 32K | 0 (11K written) | 0 |
| 2 | 52K | 11K | 0 |
| 3 | 71K | 11K | 31K |
| 4 | 91K | 11K | 49K |
| 5 | 111K | 11K | 70K |
| 6 | 130K | 11K | 111K |
| **Cost, all six** | | **$0.32** | **$0.19** |

Without a marker the implicit cache read most of the earlier prompt back, a
request or two behind: at 130K a request cost a quarter as much ($0.023
against $0.090), and the six together 40% less. The marker still pays while it
covers most of the prompt: on the live check's short conversation (below), no
marker at all cost $0.0158 against $0.0057, since the implicit cache read back
about 6K of the 8.5K prefix, and nothing on the first request.

So Gemini keeps the marker while the tokens after the last stable breakpoint
(the system prompt, or the compaction summary when there is one) are
estimated no larger than those up to it, and gets no breakpoint at all once
they are (`tailOutgrowsMarkedPrefix`). With the rule the live check's short
conversation keeps its marker; it cost $0.0052.

- **The tool definitions** are sent and cached with the prefix, so both the
  shared loop and the interactive driver's leaked-call finalize retry add them
  to their side (`WithStablePrefixTokens(EstimateToolDefsTokens(tools))`); the
  forced final summary sends no tools.
- **An image or other file** counts at 1,000 tokens (`mediaCacheTokens`),
  about what Gemini bills for an image, not at the base64 length the overflow
  estimator counts, which would drop the marker for any chat with a
  screenshot. A long PDF is counted low, which keeps the marker.
- **Switching.** Leaving the explicit cache, the implicit one reads nothing
  until it has warmed (two requests from cold in the probe); coming back to it
  after a compaction costs one fresh write. History is append-only between
  compactions, so the switch happens at most once a compaction cycle.

Breakpoint placement is itself part of the contract: the last-system-message
breakpoint is what makes the prefix cacheable, so the system prompt must be the
leading, stable content. For Gemini past the switch there is no breakpoint, and
the append-only history above is what its implicit cache reads.

## Live check (`TestPromptCacheLive` & `TestPromptCacheLive_LongToolChainCrossTurn`)

`internal/agentcore/prompt_cache_live_test.go` drives the real `Run` loop —
fleet's breakpoints, `openRouterCacheControlTransport` wire normalization,
upstream pins, realistic 6-tool roster (`tool_search`, `query_metrics`,
`inspect_box`, `read_runbook`, `run_python`, `render_artifact`), and persisted
`runtime_today` notes — against OpenRouter, and reports per request the served
provider, prompt / cached / cache-write tokens, hit rate, and cost.

Two tests run in the `prompt-cache-live` CI gate (`.github/workflows/ci.yml`,
required by `CI gate`, path-gated on PRs to prompt-cache-relevant changes and
always run on `main`) and the nightly canary (`.github/workflows/e2e-canary.yml`):

1. `TestPromptCacheLive`: runs a 3-turn multi-tool conversation (Turn 1 makes 3
   sequential tool calls returning ~1,200–1,400 tokens each; Turn 2 replays
   Turn 1 via `liveReplayEntries`, sends a ~1,300-token user message, and calls
   `read_runbook`; Turn 3 synthesizes across both turns). By default in CI it
   runs across the fast representative models for each explicit cache family
   (`anthropic/claude-haiku-5.5` and `google/gemini-3.8-flash`); with
   `FLEET_CACHE_LIVE_MODELS=all` (used by the nightly canary) it runs across the
   full 5-model matrix (`anthropic/claude-opus-5.5`, `anthropic/claude-haiku-5.5`,
   `google/gemini-3.8-flash`, `google/gemini-4-argon`, and
   `openai/gpt-6-luna-pro`).
2. `TestPromptCacheLive_LongToolChainCrossTurn`: runs a 10-step sequential
   pagination tool chain in Turn 1 (>20 content blocks, exceeding Anthropic's
   20-block lookback window) followed by Turn 2 after history replay, asserting
   step-by-step cache growth, all 4 breakpoints (`system` + `runtime_today` + 2
   rolling tail messages), and cross-turn cache preservation on Turn 2 Step 0.

```sh
FLEET_CACHE_LIVE=1 OPENROUTER_API_KEY=… \
  go test -tags fleet_host_executor -run TestPromptCacheLive -v ./internal/agentcore/
```

It fails when a request after the first reads nothing from cache (one miss is
allowed among the requests left to the implicit cache, the upstream's best
effort: an implicit-cache family's, a Gemini request sent with no
breakpoint, and every request to a model listed in `liveImplicitOnly` —
`google/gemini-4-argon`, which takes the stable-prefix breakpoint but has no
explicit cache, so its reads are Google's implicit cache and a request routed
to a cold replica reads nothing; every other request must read), or when
fleet's recorded cached tokens disagree with the wire. A `liveImplicitOnly`
model that reports any cache write fails the check: it has gained explicit
caching, and the list must be updated. The response stream is teed, not
buffered, so the first-chunk watchdog behaves as in production, and each turn
replays the previous turn's text, tool calls and tool results the way
`replayHistory` does. `FLEET_CACHE_LIVE_TURN_GAP`
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
| Google | `gemini-4-argon` (strong tier) | 2/3 in the matrix run; **15/15 over five repeat runs** | implicit only; reads ~4.1–6.9K of ~8.9K (Google varies it) |
| Google | `gemini-3.8-flash` | 3/3 | one write, then the full ~8.5K read; see the Gemini placement note above |
| xAI | `grok-4.7` | 3/3 | |
| Qwen | `qwen3.8-max-0902`, `qwen3.8-max-prime` | 3/3 each | implicit; explicit breakpoints measured slightly dearer |
| Z.AI | `glm-5.3`, `glm-5.3-prime` | 3/3 each | |
| Moonshot | `kimi-k3` | 3/3 | |
| StepFun | `step-5-preview` | 3/3 | |
| Xiaomi | `mimo-v2.6-pro` | 3/3 | 2/3 in an earlier run (alternating hit/miss on GMICloud) |
| DeepSeek | `deepseek-v4.1-flash` | 2/3 | served by a fallback host (Morph) this run; 3/3 on DeepSeek's own endpoint in two earlier runs |
| DeepSeek | `deepseek-v4-pro-0813` | 0/3 | served by a third-party host (Baidu), not DeepSeek's endpoint; 2/3 in an earlier run |
| MiniMax | `minimax-m3` | 2/3 | first repeat missed, then hit |
| NVIDIA | `nemotron-3-ultra-550b-a55b` | 2/3 | alternating hit/miss on one provider (BaseTen) |
| Meta | `muse-wonder` | 2/3 | intermittent upstream |
| Meta | `muse-spark-1.3` | 3/3 | ~113 tokens only |
| Mistral | `mistral-large-4-0` | 1/3 | implicit cache warms only after several requests |
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
it, turn 2 read ~6.5K), and a six-turn chat cost ~60–75% more input. The
turn-2 request missed in all four live runs on the old layout; with the note
persisted in history it read from cache in all five repeat runs (15/15 later
requests).

**Known gap: cache writes are not recorded.** fantasy's OpenRouter usage
struct has no `cache_write_tokens` field, so `CacheCreationTokens` is 0 for
every OpenRouter model (the live check logs the difference instead of
failing). Default billing is unaffected — fleet uses OpenRouter's reported
`usage.cost` — but per-model pricing overrides that price cache writes, and the
`cache_creation_tokens` shown in session and task stats, under-count. The fix
belongs upstream in fantasy (parse `prompt_tokens_details.cache_write_tokens`
into `fantasy.Usage.CacheCreationTokens`).

## Kill switch & observability

- **Kill switch:** `FLEET_DISABLE_PROMPT_CACHE` makes `PromptCachingStep` a
  no-op (the `CHAT_`/`CUTLASS_` spellings were removed, ADR-0106).
- **Observability:** cached-token counts are tracked (`CachedTokens`) and surfaced
  in the admin stats / cost accounting, so an operator can watch the cache-hit
  ratio and notice a regression the guard test didn't catch (e.g. a system-prompt
  volatility that only manifests live).
