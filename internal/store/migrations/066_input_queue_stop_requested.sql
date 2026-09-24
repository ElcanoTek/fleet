-- Durable Stop-by-key intent. A Stop naming an input's idempotency key
-- (POST /conversations/{id}/cancel with input_id) stamps stop_requested_at on
-- the key's pending row before it cancels the turn running it. Turn-end
-- settlement and boot recovery then cancel such a row, when its user entry
-- (or steered text) never committed, instead of returning it to the queue:
-- the in-memory record of the Stop does not survive a restart, and a
-- re-queued row would let a later drain run an input its Stop was answered
-- for. NULL — no Stop named the key, or rows written by an older binary — is
-- the previous behaviour.
ALTER TABLE chat_input_queue ADD COLUMN stop_requested_at BIGINT;
