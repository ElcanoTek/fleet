-- Idempotency keys for creating a conversation (POST /conversations with
-- client_request_id): a client that lost the reply to a create retries with
-- the same key and gets the chat it already made, instead of a second one.
-- The Patrol Launchpad uses it so a lost reply can never orphan a chat
-- (docs/PATROL-LAUNCHPAD.md). A key is recorded only after the whole create
-- succeeded; keys older than a day are pruned as new ones are written, and an
-- account's keys go with the account.
CREATE TABLE IF NOT EXISTS conversation_create_keys (
    user_email        TEXT NOT NULL,
    client_request_id TEXT NOT NULL,
    conversation_id   TEXT NOT NULL,
    created_at        BIGINT NOT NULL,
    PRIMARY KEY (user_email, client_request_id)
);
