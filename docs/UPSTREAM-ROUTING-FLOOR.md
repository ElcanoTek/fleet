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

**The default has since moved to `google/gemini-3.7-flash`**, which Google
serves alone and which is therefore pinned *strictly* (`Only`, no fallbacks) —
one upstream means no pool to vary precision across, so it needs no floor. The
floor below is unchanged and still applies to the DeepSeek family: those slugs
remain selectable, and the pin plus the floor are what make selecting them safe.
`TestDefaultCoreModelCannotBeServedAtArbitraryPrecision` now asserts the general
property — whichever family holds the default slot is either strictly pinned or
carries a floor — so a future default swap cannot silently drop the guarantee.

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

- **The floor is a request-level preference, not an enforced guarantee.** It is
  passed to OpenRouter as `provider.quantizations`; fleet cannot verify what
  precision actually served a request, because OpenRouter's response metadata
  reports the provider name, not the quant level. The attribution above tells
  you *which upstream* answered, which is the actionable signal — confirming its
  precision means looking that endpoint up in OpenRouter's catalog.
- **This is diagnosis, not enforcement.** `ServedFallback` is recorded and
  logged. Nothing refuses a run, retries on a different route, or surfaces the
  flag in the chat UI or the task page. Wiring it to an Observer/metric and
  showing it next to the cost chip is a follow-on.
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
  share backing state with the table), `TestPreferredUpstreamFor`.
- `internal/agentcore/served_upstream_test.go` — canonical route is not a
  fallback, an off-pin route latches the flag, unpinned families never flag,
  and absent metadata preserves the last known attribution.
