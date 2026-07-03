# Admin-managed LLM providers

Admins can add, edit, disable, and remove LLM providers from the web admin
page (Settings → Admin → Model providers) — an OpenRouter, Anthropic, or
OpenAI API key, or any OpenAI-compatible endpoint (Ollama, vLLM, LM Studio, a
gateway). Multiple providers run simultaneously; edits apply on the next
message with **no restart**. This builds on the #289 multi-provider routing
layer (`internal/agentcore/multiprovider.go`), which previously could only be
configured from the client bundle's `providers:` block.

## What shipped

- **Storage**: `llm_providers` (chat-DB migration 034) — name (the routing
  prefix), type (`openrouter|anthropic|openai|ollama`), base URL, JSONB models
  list, enabled flag, and the API key sealed with the store's secretbox cipher
  (AES-256-GCM, `FLEET_MCP_OAUTH_ENCRYPTION_KEY`), AAD-bound to the row id.
  `internal/store/llm_providers.go`.
- **API** (chat server): admin-gated CRUD at `/admin/llm-providers[/{id}]`
  (same `adminMiddleware` as the rest of `/admin/*`), plus a member-level
  `GET /llm-provider-models` that returns enabled providers' model slugs for
  the picker. Web proxies under `/api/admin/llm-providers` and
  `/api/llm-provider-models`.
- **Routing table**: at boot and after every admin edit, the resolver's table
  is `MergeLLMProviders(bundle providers, admin rows, env OpenRouter key)`
  (`internal/agentcore/provider_merge.go`):
  - No bundle `providers:` block + `OPENROUTER_API_KEY` set → the implicit
    single catch-all OpenRouter provider (the historical default) is the base.
  - Admin rows overlay the base: a row with the **same name replaces** the
    base entry in place (rotate the OpenRouter key from the UI); new names
    append. A provider's explicit models list always beats a catch-all
    regardless of order, so adding a listed provider never breaks other models.
  - Ties are deterministic: when two providers both list the same model slug,
    the **first in table order wins** (bundle order, then admin rows by
    creation time). Explicit `<name>/<model>` routing always bypasses the tie.
  - Provider **names are immutable** after creation — the name is the routing
    prefix baked into saved conversations' and scheduled tasks' model slugs,
    so a rename would silently break them. Delete + recreate to change one.
  - Everything empty → the historical `OPENROUTER_API_KEY required` boot error.
- **Hot swap**: `agent.Manager.SetLLMProviders` rebuilds the model resolver
  and swaps it under a lock (the same discipline as the MCP gating hot-reload).
  Interactive turns, scheduled runs, the end-of-run verifier, and phone-a-friend
  all resolve through the manager, so one swap covers them all. The swap is
  all-or-nothing: a table that fails eager construction leaves the current one
  serving, and a DB overlay that fails at **boot** degrades to the bundle/env
  table with a loud log — a bad row can never take the box down.
- **Model picker**: enabled providers' listed models are unioned into the
  shared picker (chat + task form) as `<provider>/<model>` entries with a
  "Workspace" badge (`web/src/app/shared/lib/models.ts`), ahead of the
  OpenRouter catalog. Explicit prefix routing means a picked entry resolves
  through its provider even when slugs overlap.

## Security invariants (unchanged, extended)

- **Keys are write-only.** No read endpoint returns a stored key: lists carry
  `has_api_key` only, the edit form's key field starts empty ("leave blank to
  keep"), and the one decrypting read (`LLMProviderConfigs`) exists solely to
  build the resolver host-side. Mirrors MCP credential accounts.
- **Keys stay host-side** — sealed in the DB, decrypted in-process for client
  construction, never in the sandbox, the model context, logs, or any HTTP
  response.
- Storing a key requires the secretbox cipher: deployments without
  `FLEET_MCP_OAUTH_ENCRYPTION_KEY` fail closed on keyed writes (Ollama rows,
  which need no key, still work).
- **Key scope + backups**: `FLEET_MCP_OAUTH_ENCRYPTION_KEY` now protects ALL
  at-rest secrets — MCP OAuth tokens AND provider API keys (ciphertexts are
  AAD-domain-separated, so one can never be opened as the other). Back the key
  up with the database; rotating or losing it makes stored provider keys
  undecryptable — boot then degrades to the bundle/env provider table with a
  loud log, and the admin re-enters keys in the UI.
- **Admin trust**: an admin can point a catch-all provider's `base_url`
  anywhere, routing prompts/completions through that endpoint. This is the
  same trust already vested in admins (they manage MCP credentials and the
  allowlist); base URLs are restricted to http(s) with no embedded
  credentials, and every change is visible in the panel.
- Mutating web routes enforce a same-origin check (`verifyOrigin`) on top of
  the SameSite=Lax session cookie.

## What was deliberately deferred

- **No test-connection probe.** Validation is eager client construction
  (shape-level: type, key presence, base URL); a wrong key surfaces on first
  use as a normal model-selection error. A host-side `/models` probe per type
  is a natural follow-up.
- **Catch-all providers aren't enumerated in the picker** — an empty models
  list serves any slug but contributes no picker rows (nothing to enumerate);
  users can still type any slug.
- **No per-model pricing/capability metadata for admin-provider models** —
  picker entries carry name + slug only; the OpenRouter catalog keeps its
  pricing-based filtering for its own entries.
- **The fake-LLM seam is untouched**: with no admin rows the resolver is
  byte-identical to before, and `OPENROUTER_BASE_URL` still reroutes the
  OpenRouter provider (tests and existing deployments are unchanged).
