-- 032_user_connector_prefs.sql — per-user connector availability preferences
-- (the "enabled for me" layer of the unified connector UX).
--
-- One row per (user, connector) the user has EXPLICITLY set. Absence of a row
-- means "operator default" (an optional bundled server's enabled_by_default;
-- a remote/shared connection defaults to enabled), so shipping this table
-- changes nothing until a user touches a toggle. This is a PREFERENCE, not an
-- authority boundary: the credential allowlist (Gate-3) and the sharing
-- grants remain the security gates — prefs shape what pickers offer and which
-- account seat a chat turn uses by default.
--
--   connector_kind:  'bundled' (sandboxed manifest connector, keyed by server
--                    name) | 'remote' (per-user hosted connection or a
--                    connection shared with this user, keyed by server id).
--   default_account: bundled connectors only — the credential-account seat
--                    ('' = the connector's default seat) chat turns use.
CREATE TABLE IF NOT EXISTS user_connector_prefs (
    user_email      TEXT NOT NULL,
    connector_kind  TEXT NOT NULL,
    connector_id    TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL,
    default_account TEXT NOT NULL DEFAULT '',
    updated_at      BIGINT NOT NULL,
    PRIMARY KEY (user_email, connector_kind, connector_id)
);
