-- 003_optional_mcp_servers.sql — per-conversation opt-in list for optional MCP servers.
--
-- MCP servers fall into two classes:
--
--   * Default-on  (e.g. sendgrid, email, fast_io) — always available.
--   * Optional    (e.g. gamma)                    — only loaded for a turn
--                                                   when the user has opted
--                                                   in for this conversation.
--
-- The opt-in state lives on the conversation row so the setting follows the
-- conversation (not the user account, not the session) — same scope as the
-- model picker and the persona picker.
--
-- Stored as JSONB (not TEXT[]) so that the existing database/sql plumbing —
-- which scans into []byte / json.Marshal — carries over cleanly without
-- introducing lib/pq or pgtype-specific plumbing. The column holds a JSON
-- array of lowercase server names, e.g. '["gamma"]'. The default empty
-- array means "no optional servers enabled"; existing rows get the default
-- on migration-apply so no explicit backfill is needed.

ALTER TABLE conversations
  ADD COLUMN optional_mcp_servers_enabled JSONB NOT NULL DEFAULT '[]'::jsonb;
