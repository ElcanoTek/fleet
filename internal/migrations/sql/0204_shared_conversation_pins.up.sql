-- 0204_shared_conversation_pins: a person's own pins on chats other people
-- own (a teammate's shared chat, a Multiplayer chat they take part in).
--
-- chat.conversations.pinned is the OWNER's pin: it keeps the owner's chat out
-- of the retention sweep and puts it in the owner's Pinned group. A teammate
-- who lives in that chat wants it at the top of their own list too, without
-- changing anything for the owner, so their pin is a row here instead. It is
-- a view preference only: it grants no access (the shared listing joins it to
-- the chats the person can still open) and it never keeps a chat alive.
CREATE TABLE IF NOT EXISTS chat.shared_conversation_pins (
    user_email      TEXT   NOT NULL,
    conversation_id TEXT   NOT NULL REFERENCES chat.conversations(id) ON DELETE CASCADE,
    pinned_at       BIGINT NOT NULL,
    PRIMARY KEY (user_email, conversation_id)
);
