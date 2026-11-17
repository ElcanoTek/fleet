-- 0202_user_home_conversation: each person's Home chat (docs/HOME-CHAT.md).
--
-- One row per person, so "exactly one Home" is the primary key rather than a
-- rule every writer has to remember. The chat may be one they own, a
-- Multiplayer chat they take part in, or a chat a teammate shared with their
-- team; whether they can still open it is decided when it is read, never
-- stored, so a revoked share hides Home and a re-share brings it back.
--
-- Both references cascade. The account: a new account later created under
-- the same address must not inherit someone else's Home. The chat: a hard
-- delete (the retention sweep, a purge) takes the row with it, while a soft
-- delete leaves it for the read-time check to hide.
CREATE TABLE IF NOT EXISTS chat.user_home_conversation (
    user_email      TEXT PRIMARY KEY REFERENCES core.users(email) ON DELETE CASCADE,
    conversation_id TEXT NOT NULL REFERENCES chat.conversations(id) ON DELETE CASCADE,
    updated_at      BIGINT NOT NULL
);

-- The cascade from chat.conversations looks rows up by conversation.
CREATE INDEX IF NOT EXISTS idx_user_home_conversation_conversation
    ON chat.user_home_conversation (conversation_id);
