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
**`openai/gpt-6-luna-pro`** / **`anthropic/claude-opus-5.5`** (see
[MODEL-DEFAULTS.md](MODEL-DEFAULTS.md)). Both are soft-pinned to their vendor
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
| `openai/gpt-6-luna-pro` (default) | OpenAI | OpenAI, Azure, Amazon Bedrock |
| `anthropic/claude-opus-5.5` (strong) | Anthropic | Anthropic, Claude Platform on AWS, Amazon Bedrock, Azure, Google |
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
- **Only the DeepSeek family gets a floor.** It is the one family documented to
  mix precisions across its pool. Other soft-pinned families
  (`anthropic/`, `openai/`, `moonshotai/`, `z-ai/`) were left untouched rather
  than speculatively constrained — a too-narrow allow-list makes a family
  unroutable, which is a worse failure than the one being fixed.
- **The floor may narrow availability.** If DeepSeek's first-party endpoint is
  down *and* every fp8+ alternative is saturated, a request that previously
  degraded to fp4 now fails instead. That is the intended trade: the prior
  behavior returned a plausible-looking but degraded answer with no signal.
- **Not verified against live OpenRouter.** The quantization strings match
  OpenRouter's documented spelling and the `Quantizations` field on
  `openrouter.Provider`, and the routing policy is unit-tested, but no live
  request was made from this change (tests run against the fake-LLM seam with no
  API key). The first live turn on a DeepSeek slug is the real confirmation.
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
  `TestOfficialPoolAllowlistNotAliased`, and
  `TestDefaultCoreModelCannotBeServedAtArbitraryPrecision`, which now reads the
  emitted policy rather than the exemption table.
- `internal/agentcore/served_upstream_test.go` — canonical route is not a
  fallback, an off-pin route latches the flag, unpinned families never flag,
  and absent metadata preserves the last known attribution.
