-- 034_llm_providers.sql — admin-managed LLM providers (#289 follow-on).
--
-- The #289 multi-provider routing table was sourced only from the client-config
-- bundle's providers: block (keys named by env var, resolved host-side at
-- boot). This table makes providers admin-editable at runtime from the web
-- admin page: add an Anthropic/OpenAI/OpenRouter key or point at a local
-- OpenAI-compatible endpoint (type 'openai'/'ollama' + base_url) without a
-- bundle edit or restart.
--
--   api_key_sealed: secretbox AES-256-GCM ciphertext (same cipher as the
--                   remote-MCP OAuth tokens, FLEET_MCP_OAUTH_ENCRYPTION_KEY),
--                   AAD-bound to this row's id. NULL = no key stored (Ollama
--                   and some local endpoints need none). The PLAINTEXT value
--                   is write-only through the API — no read endpoint returns
--                   it, mirroring MCP credential accounts.
--   models:         JSONB array of model slugs this provider serves; empty
--                   array = catch-all (matches any slug), same semantics as
--                   the bundle block.
--   name:           the routing prefix ("<name>/<model>" selects this
--                   provider explicitly); unique, lowercase slug.
CREATE TABLE IF NOT EXISTS llm_providers (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    provider_type  TEXT NOT NULL,
    base_url       TEXT NOT NULL DEFAULT '',
    api_key_sealed BYTEA,
    models         JSONB NOT NULL DEFAULT '[]'::jsonb,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL
);
