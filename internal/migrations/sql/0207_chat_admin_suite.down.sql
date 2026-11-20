DROP TABLE IF EXISTS chat.admin_requests;
DROP TABLE IF EXISTS chat.admin_actions;
-- Administrator messages that never ran cannot be expressed without the mode.
DELETE FROM chat.chat_input_queue WHERE mode = 'admin';
ALTER TABLE chat.chat_input_queue DROP COLUMN IF EXISTS sender_email;
ALTER TABLE chat.chat_input_queue DROP CONSTRAINT IF EXISTS chat_input_queue_mode_check;
ALTER TABLE chat.chat_input_queue
  ADD CONSTRAINT chat_input_queue_mode_check CHECK (mode IN ('queued', 'steer', 'direct', 'resume', 'patrol')) NOT VALID;
