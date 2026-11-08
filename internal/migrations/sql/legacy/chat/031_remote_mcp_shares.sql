-- 031_remote_mcp_shares.sql — share a connected remote MCP server with other
-- users on the box (#443 follow-up).
--
-- A grant row lets grantee (a user email, or '*' = everyone on this box) USE
-- the owner's connection: the grantee's chat turns and scheduled tasks mount
-- the server's tools, and tool calls authenticate with the OWNER's OAuth
-- token, brokered host-side exactly like the owner's own runs. The grantee
-- never sees the token (secrets stay sealed to the owner row); deleting the
-- grant — or the server, via the cascade — revokes access immediately because
-- grants resolve fresh per run.
CREATE TABLE IF NOT EXISTS remote_mcp_shares (
    server_id  TEXT NOT NULL REFERENCES remote_mcp_servers (id) ON DELETE CASCADE,
    grantee    TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (server_id, grantee)
);
CREATE INDEX IF NOT EXISTS idx_remote_mcp_shares_grantee ON remote_mcp_shares (grantee);
