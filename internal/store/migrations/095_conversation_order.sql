-- The order a person has given their chats in the rail
-- (docs/CHAT-ORDER.md): one row per user and scope, holding conversation
-- IDs first to last as a JSON array. A scope is "pinned" (their pinned chats)
-- or "project:<project id>" (their chats in that project). A chat in the scope
-- that the list does not name (pinned or filed since the last reorder) sorts
-- above the ordered ones; an ID that has since left the scope, or no longer
-- exists, is ignored on read. A row is removed when it would be empty, with
-- its project, and with the account. Kept apart from the conversations table
-- so a reorder is one write rather than one row update per chat.
CREATE TABLE IF NOT EXISTS conversation_order (
    user_email       TEXT NOT NULL,
    scope            TEXT NOT NULL,
    conversation_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at       BIGINT NOT NULL,
    PRIMARY KEY (user_email, scope)
);
