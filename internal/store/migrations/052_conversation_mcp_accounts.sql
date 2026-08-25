-- 052_conversation_mcp_accounts.sql — per-conversation credential-seat
-- override for the chat Tools picker (#988).
--
-- optional_mcp_servers_enabled says WHICH optional connectors a conversation
-- opted into; the seat each one runs as came only from the user's
-- connections-page default (bundled default_account) or, for hosted
-- connections, the row flagged is_default. This column lets one conversation
-- pick a different seat without changing the user's default: a JSON object of
-- server name → account label. A missing key (or '') means "the default".
-- Bundled and remote connectors share the shape; the turn path validates each
-- entry against the live seat catalog and drops stale ones rather than
-- failing the turn. Scheduled tasks are unaffected — they pin their own
-- {server, account} in mcp_selection.
ALTER TABLE conversations
    ADD COLUMN mcp_accounts JSONB NOT NULL DEFAULT '{}'::jsonb;
