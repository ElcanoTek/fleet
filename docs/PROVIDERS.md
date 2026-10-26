# Multi-provider LLM configuration (#289)

By default fleet routes every model through **OpenRouter**. A client bundle can
instead declare a `providers:` block in `manifest.yaml` to route inference
natively to **Anthropic**, **OpenAI**, a self-hosted **Ollama**, any other
**OpenAI-compatible** server (vLLM, LM Studio, llama.cpp, a hosted gateway), the
**Vercel AI Gateway**, an **Azure OpenAI** resource, or any mix — for failover,
data residency, or to avoid the OpenRouter markup. Admins can add the same
provider types from the web UI without a restart; see
[`LLM-PROVIDERS.md`](LLM-PROVIDERS.md).

**A bundle with no `providers:` block is unchanged** — a single catch-all
OpenRouter provider built from `OPENROUTER_API_KEY`, exactly as before.

See [`docs/adr/0017-multi-provider-llm.md`](adr/0017-multi-provider-llm.md) for
the design and its honest-scope limits.

## Configure

```yaml
providers:
  # Catch-all: anything not matched below goes here. OpenRouter accepts any slug.
  - name: openrouter
    type: openrouter
    api_key_env: OPENROUTER_API_KEY

  # Route specific Claude slugs straight to the Anthropic API.
  - name: anthropic-direct
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
    models: ["claude-opus-4-8", "claude-sonnet-4-6"]
    context_window_tokens: 200000

  # Route specific OpenAI slugs straight to the OpenAI API.
  - name: openai-direct
    type: openai
    api_key_env: OPENAI_API_KEY
    models: ["gpt-4o", "gpt-4o-mini"]
    context_window_tokens: 128000

  # A self-hosted Ollama server (OpenAI-compatible endpoint; no key needed).
  - name: local
    type: ollama
    base_url: http://localhost:11434/v1
    models: ["llama3.2", "qwen2.5-coder"]
    context_window_tokens: 65536 # the server's num_ctx, raised to fit fleet's tools

  # Any other OpenAI-compatible server. base_url is required; a key is optional.
  - name: gpu-box
    type: openaicompat
    base_url: http://gpu-box:8000/v1
    models: ["qwen3-coder"]
    context_window_tokens: 131072 # the server's --max-model-len

  # The Vercel AI Gateway. Model ids are <creator>/<model>.
  - name: gateway
    type: vercel
    api_key_env: AI_GATEWAY_API_KEY
    models: ["anthropic/claude-opus-4.8"]
    context_window_tokens: 200000

  # An Azure OpenAI resource. Its models are the resource's deployment names.
  - name: azure-east
    type: azure
    api_key_env: AZURE_OPENAI_API_KEY
    base_url: https://contoso.openai.azure.com
    models: ["gpt-5-prod"]
    context_window_tokens: 400000

# Optional ordered availability chain. Each target must serve the same slug.
fallback_providers: ["anthropic-direct", "openrouter"]
```

Set the credentials in `.env` (each `api_key_env` name is auto-allowlisted from
the manifest, like an MCP connector credential):

```
OPENROUTER_API_KEY=…
ANTHROPIC_API_KEY=…
OPENAI_API_KEY=…
AI_GATEWAY_API_KEY=…
AZURE_OPENAI_API_KEY=…
```

### Fields

| Field | Required | Notes |
|-------|----------|-------|
| `name` | yes | Routing name, unique within the manifest. Also the `<name>/` explicit-routing prefix. |
| `type` | yes | One of the [provider types](#provider-types) below. Any other value fails the bundle load with the list of supported types. |
| `api_key_env` | per type | Env var holding the credential (value read host-side, never in the manifest). Required for every type except `ollama` and `openaicompat`. |
| `base_url` | per type | Endpoint override (e.g. the Ollama URL). Required for `openaicompat` and `azure`, which have no default endpoint. |
| `models` | no | Slugs this provider serves. Empty = catch-all (matches any slug). |
| `context_window_tokens` | no | Actual input-plus-output context limit for provider-local models. Set this to Ollama's `num_ctx` (and for custom OpenAI-compatible gateways). Omitted Ollama entries fail safe at 4096 tokens; other native entries use a conservative 32000-token fallback. Both fallbacks are below what a turn reserves with the default tool set (a little under 35,000 tokens when this was written), so in practice every non-OpenRouter entry needs this, and boot logs a warning for each one without it, and a declared window below that reserve fails the same way: raise Ollama's `num_ctx` rather than declaring a small one. Admin-managed rows have the same field, required before a non-OpenRouter row can be enabled ([`LLM-PROVIDERS.md`](LLM-PROVIDERS.md)). OpenRouter ignores this field: observed provider limits and its public per-model catalog are more authoritative than a manifest-wide declaration. |

## Provider types

Each type is one of the provider packages in
[fantasy](https://github.com/charmbracelet/fantasy), the model library fleet
runs on. The bundle loader, the admin API and the model resolver read the
same table of types and their required fields (`providerTypeSpecs` in
`internal/agentcore/multiprovider.go`). The one difference is timing: an admin
row may be saved disabled before its key is entered, and the key is required
once the row is enabled.

| `type` | Reaches | `api_key_env` | `base_url` | Notes |
|--------|---------|---------------|------------|-------|
| `openrouter` | OpenRouter | required | not used | Accepts any slug, so it is the usual catch-all. Turns always go to OpenRouter; only the `OPENROUTER_BASE_URL` fake-LLM seam reroutes them, and a `base_url` on the entry is ignored. |
| `anthropic` | The Anthropic API | required | optional | Native Anthropic model ids (`claude-opus-4-8`). |
| `openai` | The OpenAI API | required | optional | A `base_url` points it at another server, but for anything other than OpenAI itself prefer `openaicompat`. |
| `ollama` | An Ollama server | optional | optional, default `http://localhost:11434/v1` | The ids Ollama has pulled (`llama3.2`). |
| `openaicompat` | Any OpenAI-compatible server | optional | **required** | Uses fantasy's OpenAI-compatible provider, which also reads the non-OpenAI fields such servers send, like `reasoning_content`. With no key, the request carries no `Authorization` header. |
| `vercel` | The Vercel AI Gateway | required | optional, default `https://ai-gateway.vercel.sh/v1` | Model ids are `<creator>/<model>` (`anthropic/claude-opus-4.8`), the same shape as OpenRouter slugs, so listing one takes it over from OpenRouter (see [Vercel and OpenRouter slugs](#vercel-and-openrouter-slugs)). Fleet does not read the gateway's per-model context limits, so declare `context_window_tokens`. |
| `azure` | An Azure OpenAI resource | required | **required**: the resource's endpoint | Calls the resource's v1 API, with the key in the `Api-Key` header. An endpoint on one of Azure's public-cloud hosts (`https://<resource>.openai.azure.com`, `.cognitiveservices.azure.com` or `.services.ai.azure.com`, with any path) becomes `https://<resource>.openai.azure.com/openai/v1`. Any other URL is used as given, so for a proxy in front of the resource, or an Azure cloud other than the public one, give the full API root, ending in `/openai/v1`. Model ids are the resource's deployment names. fantasy recognises a reasoning model (GPT-5, the o-series, gpt-oss) by its id alone, so name such a deployment after its model (`gpt-5-prod`, `o4-mini`): under another name it is sent `max_tokens` and a temperature, which Azure rejects for those models, so every turn on it fails. Keys only: Microsoft Entra ID sign-in is not supported. The connection test checks the endpoint and the key, not the deployment names, so it does not catch a misnamed reasoning deployment. |

The OpenAI Go client under `openaicompat`, `vercel` and `azure` copies the
host's own OpenAI settings onto every request: `OPENAI_API_KEY` or
`OPENAI_ADMIN_KEY` when it has no key, `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`,
and each header in `OPENAI_CUSTOM_HEADERS` (`OPENAI_BASE_URL` is always
overridden by the entry's own endpoint). For these three types fleet removes
all of them, so an endpoint an admin names never receives the host's OpenAI
credentials. An `azure` request carries its own key as `Api-Key` and no
`Authorization` header. A custom header named
`Content-Type`, `Accept` or `User-Agent` is the exception: the client sets those
itself, so they are left in place. The `openrouter`, `openai` and `ollama`
types still send the organization, project and custom headers, as before; an
`ollama` entry without a key sends the placeholder key `ollama`, which keeps
`OPENAI_API_KEY` off it.

fantasy also ships `bedrock`, `google` and `kronk` providers. They are not
configurable yet; issue #1703 tracks them.

## How a model slug is routed

For each requested slug, in order:

1. **Explicit `<provider-name>/<model>`** — if the part before the first `/`
   matches a configured provider `name`, route there. Write
   `anthropic-direct/claude-opus-4-8` to force the native Anthropic path.
   > Note: a plain OpenRouter slug like `anthropic/claude-opus-4.8` is **not**
   > explicit routing — `anthropic` there is an OpenRouter upstream, not a
   > provider `name`. It falls through to the catch-all (OpenRouter).
2. **`models:` list match** — the first provider whose `models` list contains the
   slug. A listed model always wins over a catch-all, wherever the catch-all sits.
3. **Catch-all** — the first provider with no `models` list.

So in the example above: `gpt-4o` → `openai-direct`; `claude-opus-4-8` →
`anthropic-direct`; `anthropic-direct/claude-opus-4-8` → `anthropic-direct`
(explicit); `anthropic/claude-sonnet-4.6` → `openrouter` (catch-all);
`anthropic/claude-opus-4.8` → `gateway` (its `models` list beats the
catch-all); `qwen3-coder` → `gpu-box`; `gpt-5-prod` → `azure-east`;
`llama3.2` → `local`.

### Vercel and OpenRouter slugs

A Vercel model id has the same shape as an OpenRouter slug, so a `vercel`
entry's `models` list can claim a slug OpenRouter would otherwise serve. Listing
`anthropic/claude-opus-4.8` on `gateway`, as the example does, moves every
conversation, scheduled task and default that names that plain slug from
OpenRouter to the gateway. Fleet does not read a cost from the gateway, so
those turns accrue $0 unless a pricing override prices the slug, and
`FLEET_MAX_COST_USD` stops bounding them. The model picker still shows the
OpenRouter catalog's price and context for the slug.

To add the gateway without moving existing work, leave its `models` list empty
and place it after the OpenRouter catch-all: only explicit routes such as
`gateway/anthropic/claude-opus-4.8` reach it then.

## Limits (see the ADR for detail)

- **OpenRouter-only features on native providers.** Upstream pinning, the
  1M-context Anthropic beta header, and extended thinking (#220) are OpenRouter
  provider options; a request routed through any other type (Anthropic,
  OpenAI, Ollama, OpenAI-compatible, Vercel, Azure) gets the base request without them
  (a documented follow-on). Vercel model ids look like OpenRouter slugs, but
  the OpenRouter options do not apply to them.
- **Native-provider cost accrues as $0** at runtime unless a #297 manifest
  pricing override prices the slug (a native response carries no OpenRouter cost
  metadata; the price table feeds only the pre-submission forecast). This
  includes `openaicompat`, `vercel` and `azure`: fleet does not read a cost
  from the Vercel AI Gateway's or Azure's responses. Because
  native runtime cost stays $0, **`FLEET_MAX_COST_USD` does not bound native
  runs** — only the token ceiling (`FLEET_MAX_TOTAL_TOKENS`) does. Add a pricing
  override to track native USD cost and re-enable the USD ceiling.
- **Cross-provider failover.** `fallback_providers` retries the same model on the
  next eligible provider after a transient failure — including mid-stream, as
  long as the failing attempt has not executed a tool (its partial text/reasoning
  is rolled back and regenerated from scratch; ADR-0035). Once a tool has run in
  the attempt, in-run failover is suppressed and the task's RetryPolicy decides.
  Explicit `provider-name/model` slugs remain pinned and
  never use the workspace chain. A scheduled task's explicit `fallback_model`
  remains a separate model substitution and takes precedence.
- **No per-task `preferred_provider` field.** Per-task pinning is available via
  the explicit `provider-name/model` slug form.
- A **misconfigured provider fails at boot**, not on first use.
- **Declare native context limits.** Native and OpenAI-compatible model handles
  do not advertise a reliable context window. Set `context_window_tokens` to the
  endpoint's real limit—especially Ollama's configured `num_ctx`—so Fleet can
  reserve tool schemas, completion headroom, and accumulated tool history before
  each provider call. The omission fallbacks are deliberately conservative, not
  claims about the model's actual capacity. For `type: openrouter`, Fleet always
  prefers a context limit learned from a provider error and then OpenRouter's
  public per-model catalog; `context_window_tokens` cannot raise that limit.
