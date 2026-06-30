-- 022_remote_mcp_servers.sql — per-user remote (hosted) MCP servers + OAuth (#443).
--
-- These tables back the "add a remote MCP server from the GUI and log in via
-- OAuth, per user" feature. Local stdio MCP servers are unaffected — they stay
-- bundle-defined with env-suffix credentials. Everything here is scoped by
-- user_email (TEXT, lowercased — same convention as memories) so one user can
-- never see or use another's connections.
--
-- SECRETS AT REST. The OAuth tokens, the DCR client_secret, and the RFC 7592
-- registration access token are stored as AES-256-GCM ciphertext (BYTEA), with
-- the AEAD's AAD bound to (user_email, canonical server URL) so a ciphertext
-- lifted from one row can't be replayed as another user's / server's. expires_at
-- is stored in cleartext so the proactive-refresh sweep can find near-expiry
-- rows without decrypting every token. See internal/secretbox.

CREATE TABLE remote_mcp_servers (
    id                            TEXT PRIMARY KEY,
    user_email                    TEXT NOT NULL,
    name                          TEXT NOT NULL,
    url                           TEXT NOT NULL,            -- canonical resource URI (the single identity)
    transport                     TEXT NOT NULL,            -- 'streamable_http' | 'sse'
    status                        TEXT NOT NULL,            -- 'login_required'|'connected'|'needs_reauth'|'error'
    status_detail                 TEXT NOT NULL DEFAULT '', -- non-secret, surfaced in the UI
    issuer                        TEXT NOT NULL DEFAULT '',
    authorization_endpoint        TEXT NOT NULL DEFAULT '',
    token_endpoint                TEXT NOT NULL DEFAULT '',
    registration_endpoint         TEXT NOT NULL DEFAULT '',
    revocation_endpoint           TEXT NOT NULL DEFAULT '',
    scopes                        TEXT NOT NULL DEFAULT '', -- space-delimited
    auth_methods                  TEXT NOT NULL DEFAULT '', -- space-delimited token_endpoint_auth_methods_supported
    client_id                     TEXT NOT NULL DEFAULT '',
    client_secret_enc             BYTEA,                    -- encrypted; NULL for a public (PKCE-only) client
    registration_access_token_enc BYTEA,                    -- encrypted; RFC 7592, for later update/delete
    created_at                    BIGINT NOT NULL,
    updated_at                    BIGINT NOT NULL
);

-- One server name per user; the (user, name) pair is the human-facing key.
CREATE UNIQUE INDEX idx_remote_mcp_servers_user_name ON remote_mcp_servers (user_email, name);
CREATE INDEX idx_remote_mcp_servers_user ON remote_mcp_servers (user_email);

CREATE TABLE remote_mcp_oauth (
    server_id            TEXT PRIMARY KEY REFERENCES remote_mcp_servers (id) ON DELETE CASCADE,
    access_token_enc     BYTEA,
    refresh_token_enc    BYTEA,
    expires_at           BIGINT NOT NULL DEFAULT 0, -- unix seconds, CLEARTEXT for the near-expiry sweep
    last_refreshed_at    BIGINT NOT NULL DEFAULT 0,
    failed_refresh_count INTEGER NOT NULL DEFAULT 0
);

-- Transient PKCE/CSRF state for an in-flight authorization. Single-use: a row is
-- deleted the moment its code is exchanged, and a background sweep clears any
-- that were abandoned past expires_at.
CREATE TABLE remote_mcp_oauth_flow (
    state             TEXT PRIMARY KEY,
    server_id         TEXT NOT NULL REFERENCES remote_mcp_servers (id) ON DELETE CASCADE,
    user_email        TEXT NOT NULL,
    code_verifier_enc BYTEA NOT NULL,
    created_at        BIGINT NOT NULL,
    expires_at        BIGINT NOT NULL
);

CREATE INDEX idx_remote_mcp_oauth_flow_expires ON remote_mcp_oauth_flow (expires_at);
