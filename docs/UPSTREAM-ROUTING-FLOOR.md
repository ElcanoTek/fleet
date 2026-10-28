# Upstream routing: precision floor + served-upstream attribution

Why a soft provider pin needs a quantization floor underneath it, and why the
run records which upstream actually answered.

## The problem this fixes

`internal/agentcore/provider.go` pins each model family to a canonical
OpenRouter upstream (`canonicalUpstream`) so the per-upstream prompt cache keeps
hitting. Two pin strengths exist:

- **strict** (`Only` + `AllowFallbacks=false`) — required for Google, whose
  encrypted thought signatures validate only at the minting upstream.
- **soft** (`Order` + `AllowFallbacks=true`) — cache locality with graceful
  degradation if the preferred endpoint is unavailable.

A soft pin states a **preference, not a constraint**. `Order` tells OpenRouter
what to try first; `AllowFallbacks=true` tells it that anything else in the pool
is acceptable when that endpoint is busy. For most families that is exactly
right — the fallback is the same model at the same precision from a different
host.

The DeepSeek family is not most families. OpenRouter serves it from 28 endpoints
whose **quantization spans fp4 to fp8**. `provider.quantizations` — the field
that constrains serving precision — was set nowhere in the repo, so the fallback
path had no quality floor at all. The pin fixed *where* requests preferred to
go and said nothing about *what precision* answered them.

This mattered more than it looks because `deepseek/deepseek-v4-flash-0731` was
`DefaultCoreModel` at the time, the recommended everyday default, so the
fallback path was the hot path for ordinary interactive chat turns. An fp4
serving of a flash-tier model does not fail loudly — it degrades into
token-level misspellings, topic drift, and runaway output, which reads as *the
model is broken* rather than *the route changed*. And because the run threw
away the served-upstream field, there was no way to tell those two apart after
the fact.

**The default moved to `google/gemini-3.8-flash`** (strict pin, one upstream, no
floor needed) and then, on 2026-09-21, to **`openai/gpt-5.6-luna-pro`** with
**`anthropic/claude-opus-5`** as the strong tier, and on 2026-09-22 to
**`openai/gpt-6-luna-pro`** / **`anthropic/claude-opus-5.5`**, and on
2026-10-14 the strong tier moved to **`google/gemini-4-argon`** (strict
`google/` pin, one upstream, so it needs neither a floor nor an
`officialPoolSlugs` entry; see [MODEL-DEFAULTS.md](MODEL-DEFAULTS.md)). The
Luna Pro and Opus slugs are soft-pinned to their vendor
with cloud resellers of the *official* weights as the only fallbacks, so neither
has a third-party quantized pool to degrade onto; `officialPoolSlugs` records
those two exact slugs with the endpoint list that was checked, and the guard test
accepts them as the third safe shape — per slug, never per family, so a future
model in the same family does not inherit the exemption. The floor below is
unchanged and still applies to the DeepSeek family (production's scheduled-task
fallback): those slugs remain selectable, and the pin plus the floor are what
make selecting them safe.
`TestDefaultCoreModelCannotBeServedAtArbitraryPrecision` asserts the general
property for both default-tier slugs — strictly pinned, a closed `only`
allow-list, or a serving-precision floor — and
`TestOfficialPoolExemptionIsNarrow` keeps the exemption off every other slug,
siblings included.

## The exemption is enforced, not snapshotted (#1589)

As first shipped, `officialPoolSlugs` was an **assertion about a pool**: a
comment recording which endpoints were seen on 2026-09-21, read by a guard test
and by nothing on the request path. `upstreamPinFor` still sent
`Order=[vendor]` with `AllowFallbacks=true` and no restriction, so if OpenRouter
later added a third-party or quantized endpoint to one of those pools, the
exemption would silently permit it — the pool can change between checks, and
nothing would notice.

The list is now the **allow-list on the request**. For a listed slug
`upstreamPinFor` emits `Only=[the validated pool]` alongside the unchanged
`Order=[vendor]` and `AllowFallbacks=true`:

| slug | `order` | `only` (provider routing names) |
|---|---|---|
| `anthropic/claude-haiku-5.5` (default since 2026-10-15) | Anthropic | Anthropic, Claude Platform on AWS, Amazon Bedrock, Azure, Google |
| `anthropic/claude-opus-5.5` (strong tier, restored 2026-10-28) | Anthropic | Anthropic, Claude Platform on AWS, Amazon Bedrock, Azure, Google |
| `openai/gpt-6-luna-pro` (default until 2026-10-15) | OpenAI | OpenAI, Azure, Amazon Bedrock |
| `openai/gpt-5.6-luna-pro` | OpenAI | OpenAI, Azure, Amazon Bedrock |
| `anthropic/claude-opus-5` | Anthropic | Anthropic, Claude Platform on AWS, Amazon Bedrock, Azure, Google |

The previous tiers keep their rows: the slugs stay selectable and an admin tier
override may still point at them.

`order` still buys prompt-cache locality; `only` closes the set the fallback may
reach. Be precise about *what* it closes, because the two are easy to conflate:
**`only` is a PROVIDER allow-list, not an endpoint allow-list.** OpenRouter's
`provider.only` matches on the provider's routing name, and the endpoint counts
below show several endpoints collapsing onto one name (OpenAI ×3, Azure ×2). So:

- **A provider not on the list cannot be routed to at all.** That is the real
  guarantee, and it is the one the exemption needs: a *third-party* host
  appearing in the pool tomorrow — the case that would actually put unofficial
  or quantized weights behind a default-tier slug — is refused at request time
  instead of silently inheriting the exemption.
- **An allow-listed provider adding or re-quantizing an endpoint still
  matches.** If OpenAI ships a fourth `gpt-5.6-luna-pro` endpoint at a different
  serving precision, `Only=[OpenAI, …]` routes to it. `only` does not pin
  endpoint-level attributes, and nothing here re-reads them.

That residual is deliberate and it is not closable with this mechanism: the
quantization floor is the endpoint-level control, and it cannot be used on these
slugs because every endpoint in both pools reports quantization `unknown`, which
`fp8AndAbove` excludes — a floor would make both default slugs unroutable. What
remains is therefore an assumption, stated plainly rather than implied away: an
allow-listed vendor or its official cloud resellers keep serving the vendor's
official weights. The exemption is trust in *those named parties*, enforced
against everyone else. Nothing re-reads endpoint attributes at runtime or in
CI, so that assumption is not machine-checked — it is carried by the recorded
snapshot and the date beside it.

**What `only` costs in availability — the check the issue asked for.** Both
pools were re-read from `GET /api/v1/models/{slug}/endpoints` on 2026-09-22:

- `openai/gpt-5.6-luna-pro` — 5 endpoints: OpenAI ×3, Azure ×2.
  (Amazon Bedrock was in the 2026-09-21 snapshot and is not in today's; it stays
  on the allow-list so its return needs no code change.)
- `anthropic/claude-opus-5` — 11 endpoints: Anthropic ×2, Claude Platform on
  AWS ×1, Amazon Bedrock ×3, Azure ×2, Google ×3.
- `anthropic/claude-haiku-5.5` (read 2026-10-15) — 10 endpoints: Anthropic ×1,
  Claude Platform on AWS ×1, Amazon Bedrock ×3 (global, us-east-1, eu-west-1),
  Azure ×2 (global, us), Google ×3 (Vertex global, europe, us); quantization
  "unknown" on all.
- `openai/gpt-6-luna-pro` — 3 endpoints, all OpenAI (`openai`, `openai/flex`,
  `openai/fast`). No Azure yet, so the everyday tier currently has a single
  provider behind it; Azure/Bedrock stay allow-listed for when they list it.
- `anthropic/claude-opus-5.5` — 11 endpoints, the same shape as Opus 5:
  Anthropic ×2 (`anthropic`, `anthropic/fast`), Claude Platform on AWS ×1,
  Amazon Bedrock ×3, Azure ×2, Google ×3.

Every endpoint in both pools is on the allow-list, so **`only` excludes nothing
that exists today**: availability is identical to the previous open fallback,
and the narrowing applies only to endpoints that appear later. That is why the
allow-list is the whole validated pool rather than the issue's literal
`Only=[vendor]` — `Only=[OpenAI]` would drop 2 of 5 Luna Pro endpoints and
`Only=[Anthropic]` 9 of 11 Opus 5 ones, which is the availability loss the
reseller fallback exists to avoid.

The issue's other option — drop the exemption and floor these slugs instead —
is not available: **every endpoint in both pools reports `quantization:
"unknown"`**, and `fp8AndAbove` deliberately excludes `unknown`, so a floor
would make both default-tier slugs unroutable. The allow-list is the form the
guarantee can actually take for these pools.

The names in the allow-list must be OpenRouter's own routing names —
`provider_name` as the endpoints API spells it (`Claude Platform on AWS`, not
`AWS`) — because a misspelling narrows the pool rather than failing loudly.
`TestOfficialPoolExemptionIsEnforcedAtRequestTime` pins the emitted `only` per
slug (and for the `~` alias form), that fallbacks stay enabled, that the
preferred upstream is inside the set it may route to, and that siblings get no
allow-list at all.

## What shipped

**1. A serving-precision floor on the pin table.** `canonicalUpstream` entries
gained a `quantizations` field. The DeepSeek entry carries `fp8AndAbove` —
`fp8`, `fp16`, `bf16`, `fp32`. `upstreamPinFor` copies it onto the returned
`openrouter.Provider` (a copy, not the shared backing array, so a caller
mutating one request's policy cannot corrupt the floor process-wide).

- `unknown` is deliberately **excluded**. An endpoint that does not declare its
  precision cannot be shown to clear the floor, and the floor's whole purpose is
  to hold on the fallback path, where nobody is watching.
- `fp8` is included because it is DeepSeek's own first-party serving precision —
  omitting it would make the *preferred* route unroutable.
- Every other family keeps `nil`, so their requests are byte-identical to before.

**1b. Per-slug floors (`precisionFloorSlugs`).** Some families cannot take a
floor as a whole but contain slugs that need one. `qwen/` is the case: Alibaba
alone serves most `qwen/` slugs (Qwen3.8 Max, Flash, Plus and the rest), and
every one of those endpoints reports quantization `unknown`, so a family-wide
`fp8AndAbove` would make them unroutable. Qwen's open-weights slugs are the
opposite: third-party hosts serve them at fp4 through bf16.

`precisionFloorSlugs` gives the floor to exact slugs, read with the endpoint
list in hand, on the same per-slug terms as `officialPoolSlugs`: a sibling does
not inherit it.

| slug | checked | pool | floor leaves |
|---|---|---|---|
| `qwen/qwen3.8-27b` | 2026-10-15 | 20 endpoints: fp4 ×3 (Tenstorrent, Darkbloom, ModelRun), fp8 ×9, bf16 ×1 (DeepInfra), fp16 ×1 (Cerebras), unknown ×6 (Wafer, DekaLLM, Phala, Novita, Alibaba, Cloudflare) | 11 endpoints |
| `google/gemma-4-26b-a4b-it` | 2026-10-15 | 13 endpoints: bf16 ×7 (DekaLLM, NextBit, CoreWeave, Io Net, Venice, Parasail, Novita), fp8 ×2 (DeepInfra, SiliconFlow), unknown ×4 (Darkbloom, Makora, Cloudflare, Google Vertex) | 9 endpoints |

**Open-weights slugs under a pinned family are not governed by its pin.** The
`google/` pin is strict (`Only=[Google]`, no fallbacks) because Gemini's
encrypted thought signatures validate only at the minting upstream. Gemma is
open weights that a dozen hosts serve, with no such signatures; pinned to
Google's single Vertex endpoint, `google/gemma-4-26b-a4b-it` failed 4 of 6
live calls with `429 … temporarily
rate-limited upstream` on 2026-10-15 while the unpinned, floored pool answered
6 of 6, and 5 of 5 real fleet turns with a tool completed after the change.
`unpinnedOpenWeightsPrefixes` (today `google/gemma`) makes such slugs route as
an unpinned family — the floor alone where one is recorded, nothing otherwise.

A listed slug with no `canonicalUpstream` entry gets the floor alone:
`quantizations` plus `allow_fallbacks: true`, with no `order` and no `only`, so
OpenRouter keeps balancing across every endpoint that clears the floor.
`preferredUpstreamFor` stays `""` for it, so served-upstream attribution never
flags a fallback (there is no preferred upstream to fall back from). A listed
slug inside a pinned family that has no family floor gets the floor on top of
its pin.

Why this slug: an operator pin (`FLEET_PINNED_MODELS`) shows it under a product
name for an in-house model alpha, so its output quality reads as that
product's quality. The fp4 endpoints are among the cheapest in its pool, so
price-weighted routing reaches them often without a floor. The pin is a display
alias; routing and this floor key on the real slug, so the floor holds under
any label.

**1c. One-off calls on the run's own model carry the same policy.** The
compaction summarizer (chat and scheduled) and manual summarize buy their
summary from the model the run is driving, but they build their own call, and
until now they sent it with no `provider` block at all. That bypassed every
policy on this page, the DeepSeek family floor included, on the calls that
condense a long run's history. They now attach
`agentcore.RoutingProviderOptions(slug)`, which is `upstreamPinFor` (pin,
allow-list, floor) and nothing else: the run loop's thinking and long-context
extras stay with the run loop. A slug with no policy still sends no `provider`
block.

Other one-off calls (chat titles, memory extraction and graph, error analysis,
library prompts, recurring-task suggestions, eval judging, metadata tools) still
send none. They use their own operator-configured models rather than the run's,
so they reach a floored slug only if an operator points one of those settings at
it.

**2. Served-upstream attribution.** `openrouterServedProvider` reads
`.Provider` off OpenRouter's provider metadata — the upstream that actually
served the step, which `updateUsage` previously discarded while keeping only
`.Usage.Cost`. `preferredUpstreamFor` is the read side of the pin table. When
they disagree, `orchestrationState` sets `LastServedUpstream`, latches
`ServedFallback`, and logs one line:

```
⚠️  Upstream fallback: model=<slug> pinned=<x> served=<y> (prompt cache cold; verify serving precision if output quality is off)
```

Logged per *transition*, not per step — the signal is the switch, and a
per-step line would be noise on a long run. The flag **latches**: a run that
falls back and later returns to the canonical upstream still reports that part
of it was served elsewhere.

## Honest scope

- **The floor and the allow-list are request-level directives, not guarantees
  fleet can verify.** They are passed to OpenRouter as `provider.quantizations`
  and `provider.only`; fleet cannot verify what precision actually served a
  request, because OpenRouter's response metadata reports the provider name, not
  the quant level. The provider name it *does* report is checked against the
  pin (see the attribution below), so a route outside the allow-list would show
  up there — after the fact, not as a refusal. The attribution above tells
  you *which upstream* answered, which is the actionable signal — confirming its
  precision means looking that endpoint up in OpenRouter's catalog.
- **This is diagnosis, not enforcement.** `ServedFallback` is recorded and
  logged. Nothing refuses a run, retries on a different route, or surfaces the
  flag in the chat UI or the task page.
- **The allow-list is a snapshot of membership, checked by hand.** Nothing
  re-reads OpenRouter's endpoint list at runtime or in CI, so a reseller *added*
  to a pool needs the same manual check and a code change before requests may
  reach it. That is the intended direction of the failure: the pool cannot grow
  silently, only shrink.
- **Only the DeepSeek family gets a family-wide floor.** It is the one family
  documented to mix precisions across its pool. Other soft-pinned families
  (`anthropic/`, `openai/`, `moonshotai/`, `z-ai/`) were left untouched rather
  than speculatively constrained — a too-narrow allow-list makes a family
  unroutable, which is a worse failure than the one being fixed. Individual
  slugs get a floor only through `precisionFloorSlugs`, one checked slug at a
  time (today: `qwen/qwen3.8-27b` and `google/gemma-4-26b-a4b-it`; the Gemma
  slug also routes outside the `google/` family pin, see
  `unpinnedOpenWeightsPrefixes`). Other open-weights `qwen/` slugs also mix
  precisions (fp4 or int4 appear on `qwen3.6-35b-a3b`, `qwen3.5-9b`,
  `qwen3.5-35b-a3b`, `qwen3-coder`, `qwen3-14b` as of 2026-10-15) and were left
  out of scope; add each with its endpoint list when someone relies on it.
- **The per-slug floor matches the exact slug.** A variant suffix
  (`qwen/qwen3.8-27b:nitro`) is a different slug and does not inherit it.
- **The floor may narrow availability.** If DeepSeek's first-party endpoint is
  down *and* every fp8+ alternative is saturated, a request that previously
  degraded to fp4 now fails instead. That is the intended trade: the prior
  behavior returned a plausible-looking but degraded answer with no signal.
- **Live verification: the Gemma policy only.** The routing policy is
  unit-tested everywhere and the quantization strings match OpenRouter's
  documented spelling and the `Quantizations` field on `openrouter.Provider`.
  Only `google/gemma-4-26b-a4b-it` has been checked live (2026-10-15): its
  unpinned, fp8-and-above request succeeded on 6 of 6 calls (each served by
  an fp8+ host) and on 5 of 5 real fleet turns with a tool, against 2 of 6
  under the old Google-only pin. The DeepSeek family floor and the
  `qwen/qwen3.8-27b` slug floor remain unverified live: the first live turn on
  a DeepSeek slug confirms the family floor, and a live turn on
  `qwen/qwen3.8-27b` confirms its slug floor. Check the provider that served each of its
  generations in OpenRouter's activity log: it should be one of the
  fp8-or-better endpoints in the table above, never Tenstorrent, Darkbloom or
  ModelRun. fleet does not surface it here, because the slug has no preferred
  upstream, so the fallback log line never fires for it.
- **This does not explain every bad response.** A flash-tier model given a large
  system prompt and a wide MCP tool roster can produce a poor answer on a
  perfectly good fp8 route. The attribution exists precisely so that case can be
  separated from a routing one instead of guessed at.

## Tests

- `internal/agentcore/provider_pin_test.go` — `TestUpstreamPinQuantizationFloor`
  (floor admits fp8, rejects fp4/fp6/int4/int8/unknown, unmixed families carry
  none), `TestUpstreamPinQuantizationsNotAliased` (the returned slice does not
  share backing state with the table), `TestPreferredUpstreamFor`,
  `TestOfficialPoolExemptionIsEnforcedAtRequestTime` (a listed slug's request
  carries its validated pool as `only`, alias form included, fallbacks still on,
  the preferred upstream inside the set, siblings with no allow-list),
  `TestOfficialPoolAllowlistNotAliased`, `TestPrecisionFloorSlugs` (a listed
  slug and its `~` form carry `fp8AndAbove` with fallbacks on and no `only`;
  `qwen/qwen3.8-27b` gets no `order` and no preferred upstream; Alibaba-only
  siblings get no pin at all), and
  `TestDefaultCoreModelCannotBeServedAtArbitraryPrecision`, which now reads the
  emitted policy rather than the exemption table.
- `internal/agentcore/served_upstream_test.go` — canonical route is not a
  fallback, an off-pin route latches the flag, unpinned families never flag,
  and absent metadata preserves the last known attribution.
