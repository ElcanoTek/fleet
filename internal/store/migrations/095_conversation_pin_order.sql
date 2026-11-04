-- The order a person has given their pinned chats in the rail
-- (docs/CHAT-PIN-ORDER.md): one row per user holding their pinned
-- conversation IDs, first to last, as a JSON array. A pinned chat that is not
-- in the list (pinned since the last reorder) sorts above the ordered ones;
-- an ID that is no longer pinned, or no longer exists, is ignored on read.
-- Kept apart from the conversations table so the order is one write per
-- reorder rather than one row update per chat.
CREATE TABLE IF NOT EXISTS conversation_pin_order (
    user_email       TEXT PRIMARY KEY,
    conversation_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at       BIGINT NOT NULL
);
