# Provider-aware model selection

## Problem

A native-provider deployment could advertise Fleet's compiled-in Gemini default
and the whole OpenRouter catalog while its resolver served only explicitly
listed OpenAI/Anthropic models. Bundle providers were also absent from the
workspace-model discovery API. Changing between these catalog choices did not
repair `no configured provider serves model` failures.

## Shipped

- `/llm-provider-models` reads public metadata from the active manager resolver:
  bundle providers, the implicit env-backed OpenRouter provider when applicable,
  and successfully applied admin overlays. Its DTO contains names, types and
  model lists only, never credentials or endpoint URLs. `routing_known: true`
  distinguishes authoritative empty tables from old servers / unknown state.
- `?slug=...` checks the same local routing precedence as execution without
  loading a model or making a provider request. The web model check consults it
  before OpenRouter catalog validation; native routes bypass that catalog.
- Both pickers filter recommendations and public catalog rows against the
  active table. Native catch-alls do not make arbitrary OpenRouter catalog
  models available; explicit routes and listed gateway model identifiers work.
  When a native catch-all shadows an OpenRouter catch-all, the pickers offer
  explicit `<openrouter-provider>/<catalog-slug>` alternatives instead.
  Explicit OpenRouter selections retain their catalog display name, prices and
  context metadata in chat. Task forecasts resolve the underlying catalog ID
  through the active provider table while retaining the selected route in the
  response; native or unknown provider prefixes are never stripped for pricing.
- Workspace discovery and task-picker caches expire after 30 seconds. Chat
  refreshes discovery when opening/closing the picker; task pickers refresh on
  opening. This is a bounded browser cache, not background polling.
- Chat validates defaults as well as custom selections. An unavailable saved
  selection stays visible with a **Choose a model** action. A changed selection
  is not blocked by an old selection's validation response. The backend rejects
  unroutable chat models before sandbox provisioning / MCP scope setup.

## Operator recovery

Choose an available workspace row (`<configured-provider-name>/<local-model>`)
and retry. Set `default_model` and `advanced_model` in **Settings → Admin →
Features → Model tiers** (or `FLEET_DEFAULT_MODEL` / `FLEET_ADVANCED_MODEL`) to
routes served by this deployment. Existing conversations and tasks retain their
saved model and require an explicit edit. Merely adding an OpenAI API key does
not make Google models available.

## Scope and deliberate limits

This fixes discovery and selection, not provider support. A local route check
cannot prove the remote model exists, the key has access, or the endpoint is
healthy. Native catch-all catalogs remain advisory. Typed custom model routes
remain supported, and unavailable model choices never silently switch provider.
An unsupported default requires the user to choose a workspace model and the
admin to correct the default; Fleet does not pick an arbitrary replacement.

Discovery failure / older servers retain the historical picker fallback rather
than claiming an empty table; execution is still authoritative. Scheduled task
model values and auxiliary model configuration are not automatically rewritten.
Provider edits retain the existing per-process hot-reload behavior; cross-pod
configuration propagation is outside this fix. No routing, credential, sandbox,
or approval invariants change.
