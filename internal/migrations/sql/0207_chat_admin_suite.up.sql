-- 0207_chat_admin_suite: operator administration of chats (ADR-0115).
--
-- 1. An administrator can send a message into any chat (`fleet chats send`,
--    Settings -> Chats). It is queued like a patrol run — the turn runs as
--    the chat's owner, with the owner's connectors and approval mode — but
--    its input is the administrator's words, so the queue row carries a new
--    mode ('admin') and the administrator's email (sender_email), which the
--    turn records on the user entry so every reader sees who wrote it.
-- 2. Requests that only the running server can carry out (send a message,
--    stop a turn, transfer a chat) go through chat.admin_requests: the CLI,
--    which talks to the database and not to the server, writes the request,
--    and the server executes it on its next scheduler pass with the same code
--    its own routes use (the web admin page executes it at once).
-- 3. Every operator change to a chat (a message, a notice, a title/model/
--    approval-mode/archive change, a transfer, a patrol edit) is recorded in
--    chat.admin_actions: who, through what, which chat, why.
ALTER TABLE chat.chat_input_queue DROP CONSTRAINT IF EXISTS chat_input_queue_mode_check;
ALTER TABLE chat.chat_input_queue
  ADD CONSTRAINT chat_input_queue_mode_check CHECK (mode IN ('queued', 'steer', 'direct', 'resume', 'patrol', 'admin')) NOT VALID;
ALTER TABLE chat.chat_input_queue VALIDATE CONSTRAINT chat_input_queue_mode_check;

ALTER TABLE chat.chat_input_queue
  ADD COLUMN IF NOT EXISTS sender_email TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS chat.admin_actions (
    id              BIGSERIAL PRIMARY KEY,
    created_at      BIGINT NOT NULL,
    actor           TEXT NOT NULL,
    via             TEXT NOT NULL CHECK (via IN ('cli', 'web')),
    action          TEXT NOT NULL,
    conversation_id TEXT NOT NULL DEFAULT '',
    schedule_id     TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS admin_actions_conversation_idx ON chat.admin_actions (conversation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS admin_actions_created_idx ON chat.admin_actions (created_at DESC);

CREATE TABLE IF NOT EXISTS chat.admin_requests (
    id              TEXT PRIMARY KEY,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    actor           TEXT NOT NULL,
    via             TEXT NOT NULL CHECK (via IN ('cli', 'web')),
    kind            TEXT NOT NULL CHECK (kind IN ('message', 'stop', 'transfer')),
    conversation_id TEXT NOT NULL,
    payload         TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'failed')),
    result          TEXT NOT NULL DEFAULT '',
    input_id        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS admin_requests_pending_idx ON chat.admin_requests (created_at) WHERE state = 'pending';
