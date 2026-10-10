-- Second half of 073, in its own transaction so 073's ACCESS EXCLUSIVE lock
-- is released first. VALIDATE CONSTRAINT takes SHARE UPDATE EXCLUSIVE, so
-- reads and writes keep going while existing rows are checked (every existing
-- row is 'queued', 'steer' or 'direct', which the widened check accepts).
ALTER TABLE chat_input_queue VALIDATE CONSTRAINT chat_input_queue_mode_check;
