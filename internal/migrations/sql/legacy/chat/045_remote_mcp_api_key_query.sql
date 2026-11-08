-- api_key connections: some vendors authenticate their hosted MCP server with
-- the key in a URL query parameter instead of a header (Browserbase's
-- ?browserbaseApiKey=…). This is the PARAMETER NAME only — non-secret, like
-- api_key_header; the key itself stays sealed in api_key_enc and is attached
-- per-request by the HTTP transport, never stored in the url column.
ALTER TABLE remote_mcp_servers
    ADD COLUMN api_key_query TEXT NOT NULL DEFAULT '';
