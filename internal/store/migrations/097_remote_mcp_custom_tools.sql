-- 097_remote_mcp_custom_tools.sql
-- Adds custom_tools JSONB to remote_mcp_servers so user-owned or shared custom
-- HTTP API ("http_tools") and Playwright browser ("playwright") connectors can
-- store their declarative tool specifications alongside their sealed credentials.

ALTER TABLE remote_mcp_servers
    ADD COLUMN IF NOT EXISTS custom_tools JSONB NOT NULL DEFAULT '[]'::jsonb;
