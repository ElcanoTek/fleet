# ADR-0017: Multi-provider LLM support

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

fleet routed **every** LLM call through OpenRouter as a singleton gateway. That
is a single point of failure (OpenRouter down ⇒ no inference), forces every
prompt and completion through a third-party intermediary (a data-residency
concern for GDPR/HIPAA deployments), and adds a per-token routing markup.

The `fantasy` library fleet already depends on ships native provider backends —
`providers/anthropic`, `providers/openai`, `providers/openrouter` (and others) —
and the engine is already provider-agnostic: it holds `fantasy.LanguageModel`
*interfaces* (`engine.model` / `engine.fallbackModel`), not concrete OpenRouter
types. The coupling was entirely in *how those models were constructed* — always
`newOpenRouterProvider`. So native multi-provider support is a construction-time
change, not an engine rewrite.

This does not weaken an invariant (governance stays one core; credentials stay
host-side), but it is a significant change to the model-resolution core, so it is
recorded here per the ADR convention.

## Decision

Add an optional `providers:` block to the client-bundle manifest. Each entry
names a backend (`openrouter` | `anthropic` | `openai` | `ollama`), the env var
holding its credential, an optional base-URL override, and the model slugs it
serves. The model resolver routes each slug to the right provider and builds the
model through it.

**Backward compatibility is the hard requirement.** A bundle with **no**
`providers:` block behaves exactly as before: a single catch-all OpenRouter
provider built from `OPENROUTER_API_KEY`. Existing deployments are byte-for-byte
unchanged — `NewModelResolver(apiKey, headers)` still exists and now simply
constructs that one-provider resolver.

### Routing (`agentcore.selectProvider`)

For a given model slug, in order:

1. **Explicit `<provider-name>/<model>`** — when the segment before the first
   `/` exactly matches a *configured provider name*, route there. (An OpenRouter
   slug like `anthropic/claude-opus-4.8` is **not** explicit routing unless a
   provider is literally named `anthropic`; there `anthropic` is an OpenRouter
   upstream, not a fleet provider name. So an operator writes
   `anthropic-direct/claude-opus-4-8` to pin the native path.)
2. **Implicit models-list match** — the first provider whose `models:` list
   contains the slug. A specifically-listed model beats a catch-all regardless of
   order, so a catch-all OpenRouter entry can sit anywhere.
3. **Catch-all** — the first provider with an empty `models:` list.

### Provider construction (`agentcore.buildProvider`)

- `openrouter` reuses `newOpenRouterProvider`, so the `OPENROUTER_BASE_URL` E2E
  fake-LLM seam keeps working unchanged.
- `anthropic` / `openai` use `anthropic.New` / `openai.New` with `WithAPIKey` and
  optional `WithBaseURL`.
- `ollama` has no dedicated fantasy provider; it is the OpenAI provider pointed at
  the Ollama OpenAI-compatible endpoint (default `http://localhost:11434/v1`),
  needing no real key.

All providers are built **eagerly at boot**, so a misconfigured provider fails at
startup, not on the first turn that routes to it.

### Credential boundary

`api_key_env` names an env var; its **value** is read host-side (`os.Getenv`) in
`cmd/fleet` at boot and handed to the resolver in memory — exactly like an MCP
connector credential. The env-var name is registered via `EnvVarNames →
RegisterAllowedEnvVars`, so it flows from `.env` and needs no change to fleet's
static env allowlist (provider choice is client content, kept out of the core).
The secret never lives in the manifest, the sandbox, the model context, or logs.

### Governance stays one core

The engine is unchanged. A model from any provider is a `fantasy.LanguageModel`
bound to its own provider; the resolver is the single seam that picks which
provider builds it. `agentcore.Run` remains the one governed loop.

## Consequences

- A deployment can fail over to, or route sensitive prompts directly to, a native
  provider or a self-hosted Ollama, and eliminate the OpenRouter markup.
- One new construction seam (`selectProvider` + `buildProvider` + the
  multi-provider `ModelResolver`); the engine, drivers, and host-side helpers are
  untouched (they call `resolver.Resolve` as before).

### Honest scope (not in this change)

- **OpenRouter-specific provider options are OpenRouter-only.** Upstream pinning
  (`upstreamPinFor`), the 1M-context Anthropic beta header, and extended-thinking
  activation (#220) are all built as `fantasy.ProviderOptions` keyed under
  `openrouter.Name`, so a *native* Anthropic/OpenAI model silently ignores them
  (fantasy applies options only for the model's own provider). Native-provider
  requests therefore get the base request — **no extended thinking, no upstream
  pin, no 1M header**. Translating those to `anthropic.NewProviderOptions` /
  `openai.NewProviderOptions` is a follow-on.
- **Cost accounting uses the OpenRouter price table.** A native-provider slug not
  in the pricing table falls back to the configured pricing default, so cost
  figures for native providers may be approximate until a provider-aware price
  source lands.
- **No `fallback_providers` chain.** The engine's single `fallbackModel` still
  works (and a fallback slug can now resolve to a *different* provider), but the
  issue's ordered multi-provider fallback list is not wired.
- **No per-task `preferred_provider` field.** Per-task/persona provider pinning is
  available today via the explicit `provider-name/model` slug form in a task's or
  persona's `model:`; a dedicated field is a follow-on.
- **No `FLEET_DEFAULT_PROVIDER` env knob.** Absence of the `providers:` block
  already defaults to OpenRouter, so a separate env selector would be a control
  with no additional behavior; omitted rather than faked.
- **No native-provider E2E.** The fake-LLM seam speaks the OpenRouter wire format;
  a native-provider live test needs a provider-specific fake. Routing and
  construction are unit-tested; the OpenRouter path keeps its E2E coverage.
