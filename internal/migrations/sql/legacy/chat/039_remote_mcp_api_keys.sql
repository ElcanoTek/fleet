-- 039_remote_mcp_api_keys.sql — API-key auth for per-user remote MCP servers
-- (connector-directory onboarding). Until now the per-user remote flow spoke
-- only OAuth (discovery + DCR + PKCE) or "open" (no Authorization header), so
-- every directory entry labeled `auth: api_key` was a listing with no working
-- connect path. This adds a third auth kind: the user pastes a vendor API key
-- once; it is sealed with the same AES-256-GCM cipher as the OAuth tokens
-- (AAD bound to purpose + user_email + canonical URL) and replayed host-side
-- as a static header on every MCP request. The key never enters the sandbox,
-- the model context, or any response body.
--
-- auth_kind makes the row's auth model explicit instead of inferring it from
-- issuer = '' (which conflated "open" with "not yet discovered"). Existing
-- rows are backfilled: an OAuth row always has an issuer; a row without one
-- could only have been created as an open server.

ALTER TABLE remote_mcp_servers
    ADD COLUMN auth_kind      TEXT NOT NULL DEFAULT '',
    ADD COLUMN api_key_header TEXT NOT NULL DEFAULT '', -- header NAME only (non-secret); '' = Authorization: Bearer
    ADD COLUMN api_key_enc    BYTEA;                    -- encrypted; NULL for oauth/open rows

UPDATE remote_mcp_servers
SET auth_kind = CASE WHEN issuer = '' THEN 'open' ELSE 'oauth' END
WHERE auth_kind = '';
