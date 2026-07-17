-- Conversation-owned input queue (#785). A submission that arrives while a
-- turn is running is QUEUED (durable before the API acknowledges it), never an
-- implicit cancel of the active turn — explicit /cancel stays the only Stop.
-- Queued rows drain as ordinary separate turns; mode='steer' rows are offered
-- to the running turn's PrepareStep boundary and fall back to a queued turn if
-- the turn ends first. client_input_id is the caller's idempotency key.
CREATE TABLE chat_input_queue (
  id                TEXT   PRIMARY KEY,
  conversation_id   TEXT   NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  user_email        TEXT   NOT NULL,
  client_input_id   TEXT   NOT NULL,
  message           TEXT   NOT NULL,
  attachments       TEXT   NOT NULL DEFAULT '[]',
  mode              TEXT   NOT NULL CHECK (mode IN ('queued','steer')),
  state             TEXT   NOT NULL CHECK (state IN ('queued','running','injected','completed','cancelled')),
  position          BIGINT NOT NULL,
  turn_id           TEXT,
  created_at        BIGINT NOT NULL,
  updated_at        BIGINT NOT NULL
);

-- Idempotent submission: one row per (conversation, client key); a re-POST
-- returns the existing item instead of duplicating the input.
CREATE UNIQUE INDEX chat_input_queue_idem ON chat_input_queue (conversation_id, client_input_id);

-- FIFO drain order over the pending set only.
CREATE INDEX chat_input_queue_pending ON chat_input_queue (conversation_id, position) WHERE state = 'queued';
