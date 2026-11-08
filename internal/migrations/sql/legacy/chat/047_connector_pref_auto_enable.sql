-- 047_connector_pref_auto_enable.sql — split "available to me" from "on for
-- new chats" (two-toggle connector prefs).
--
-- Until now one boolean (enabled) drove BOTH whether a bundled connector
-- appears in a user's pickers AND whether new conversations start with it
-- enabled. Those are different intents ("keep it handy" vs "always on"), so
-- rows gain auto_enable: enabled remains the availability toggle; auto_enable
-- makes new conversations start with the connector on. Absence of a row still
-- means operator defaults (available; enabled_by_default seeds new chats).
--
-- Backfill preserves observed behavior: under the old conflated semantics an
-- enabled row seeded new chats on, so existing rows get auto_enable = enabled.
ALTER TABLE user_connector_prefs ADD COLUMN auto_enable BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE user_connector_prefs SET auto_enable = enabled;
