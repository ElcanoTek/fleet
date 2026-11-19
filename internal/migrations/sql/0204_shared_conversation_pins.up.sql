-- 0204_shared_conversation_pins: a person's own pins on chats other people
-- own (a teammate's shared chat, a Multiplayer chat they take part in).
--
-- chat.conversations.pinned is the OWNER's pin: it keeps the owner's chat out
-- of the retention sweep and puts it in the owner's Pinned group. A teammate
-- who lives in that chat wants it at the top of their own list too, without
-- changing anything for the owner, so their pin is a row here instead. It is
-- a view preference only: it grants no access (the shared listing joins it to
-- the chats the person can still open) and it never keeps a chat alive.
--
-- Both ends cascade: a pin goes with the account that made it and with the
-- chat it is on, whichever is deleted first, so no request racing a deletion
-- can leave a row behind. user_email is the account's stored (lower-case)
-- email.
CREATE TABLE IF NOT EXISTS chat.shared_conversation_pins (
    user_email      TEXT   NOT NULL REFERENCES core.users(email) ON DELETE CASCADE ON UPDATE CASCADE,
    conversation_id TEXT   NOT NULL REFERENCES chat.conversations(id) ON DELETE CASCADE,
    pinned_at       BIGINT NOT NULL,
    PRIMARY KEY (user_email, conversation_id)
);

-- The primary key leads with user_email; deleting a chat cascades by
-- conversation_id alone and needs an index of its own to stay a lookup.
CREATE INDEX IF NOT EXISTS shared_conversation_pins_conversation_idx
    ON chat.shared_conversation_pins (conversation_id);
