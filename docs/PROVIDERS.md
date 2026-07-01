# Multi-provider LLM configuration (#289)

By default fleet routes every model through **OpenRouter**. A client bundle can
instead declare a `providers:` block in `manifest.yaml` to route inference
natively to **Anthropic**, **OpenAI**, a self-hosted **Ollama**, or any mix —
for failover, data residency, or to avoid the OpenRouter markup.

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

  # Route specific OpenAI slugs straight to the OpenAI API.
  - name: openai-direct
    type: openai
    api_key_env: OPENAI_API_KEY
    models: ["gpt-4o", "gpt-4o-mini"]

  # A self-hosted Ollama server (OpenAI-compatible endpoint; no key needed).
  - name: local
    type: ollama
    base_url: http://localhost:11434/v1
    models: ["llama3.2", "qwen2.5-coder"]
```

Set the credentials in `.env` (each `api_key_env` name is auto-allowlisted from
the manifest, like an MCP connector credential):

```
OPENROUTER_API_KEY=…
ANTHROPIC_API_KEY=…
OPENAI_API_KEY=…
```

### Fields

| Field | Required | Notes |
|-------|----------|-------|
| `name` | yes | Routing name, unique within the manifest. Also the `<name>/` explicit-routing prefix. |
| `type` | yes | `openrouter` \| `anthropic` \| `openai` \| `ollama`. |
| `api_key_env` | all but `ollama` | Env var holding the credential (value read host-side, never in the manifest). |
| `base_url` | no | Endpoint override (e.g. the Ollama URL, or an OpenAI-compatible gateway). |
| `models` | no | Slugs this provider serves. Empty = catch-all (matches any slug). |

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
(explicit); `anthropic/claude-opus-4.8` → `openrouter` (catch-all); `llama3.2` →
`local`.

## Limits (see the ADR for detail)

- **OpenRouter-only features on native providers.** Upstream pinning, the
  1M-context Anthropic beta header, and extended thinking (#220) are OpenRouter
  provider options; a natively-routed Anthropic/OpenAI request gets the base
  request without them (a documented follow-on).
- **Native-provider cost accrues as $0** at runtime unless a #297 manifest
  pricing override prices the slug (a native response carries no OpenRouter cost
  metadata; the price table feeds only the pre-submission forecast). Because
  native runtime cost stays $0, **`FLEET_MAX_COST_USD` does not bound native
  runs** — only the token ceiling (`FLEET_MAX_TOTAL_TOKENS`) does. Add a pricing
  override to track native USD cost and re-enable the USD ceiling.
- **No `fallback_providers` chain / per-task `preferred_provider` field** yet —
  per-task pinning is available via the explicit `provider-name/model` slug form.
- A **misconfigured provider fails at boot**, not on first use.
