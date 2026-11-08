-- 051_remote_mcp_seats.sql — multiple logins ("seats") per hosted MCP
-- connection name (#988).
--
-- Until now remote_mcp_servers was unique on (user_email, name): one OAuth
-- token / API key per user per catalog entry, so a user who wanted a work AND
-- a personal GitHub could not hold both. This adds the same seat model the
-- bundled connectors already have (<VAR>_<ACCOUNT> env seats): several rows
-- may share a name, each distinguished by a public account label and each
-- carrying its own sealed credential. Tokens are never merged — a run mounts
-- exactly one seat per name and registers it under the bundle formula
-- (`name` for the unlabeled seat, `name_<account>` for a labeled one).
--
--   account    : the seat's public label ('work', 'personal'; canonical
--                lowercase [a-z0-9_]). '' is the unlabeled seat — every
--                pre-existing connection is one.
--   is_default : the seat a user's chats mount for this name when neither
--                the conversation nor a task pins another. Exactly one per
--                (user_email, name), enforced by the partial unique index.
--                Owner-side: a grantee of a shared seat inherits the owner's
--                choice.
--
-- Backfill: every existing row is the only seat of its name, so it becomes
-- the default. Sharing stays per row (per seat): sharing "work" never exposes
-- "personal".

ALTER TABLE remote_mcp_servers
    ADD COLUMN account    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN is_default BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE remote_mcp_servers SET is_default = TRUE;

DROP INDEX IF EXISTS idx_remote_mcp_servers_user_name;
CREATE UNIQUE INDEX idx_remote_mcp_servers_user_name_account
    ON remote_mcp_servers (user_email, name, account);
CREATE UNIQUE INDEX idx_remote_mcp_servers_user_name_default
    ON remote_mcp_servers (user_email, name) WHERE is_default;
