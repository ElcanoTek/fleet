-- In-chat patrols and timers (docs/CHAT-PATROLS.md): the turn fleet starts
-- when a conversation's recurring cron patrol or one-shot timer fires is an
-- input-queue row of mode 'patrol', so patrol provenance is server-owned on
-- the queue row rather than inferred from client_input_id.
--
-- Two-step swap (same pattern as 064/065 and 073/074): replace the CHECK
-- constraint NOT VALID here and validate in 088 in its own transaction.
ALTER TABLE chat_input_queue DROP CONSTRAINT IF EXISTS chat_input_queue_mode_check;
ALTER TABLE chat_input_queue
  ADD CONSTRAINT chat_input_queue_mode_check CHECK (mode IN ('queued', 'steer', 'direct', 'resume', 'patrol')) NOT VALID;
