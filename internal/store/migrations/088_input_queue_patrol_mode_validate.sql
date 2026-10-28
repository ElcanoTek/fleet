-- Second half of 087, in its own transaction so 087's ACCESS EXCLUSIVE lock
-- is released first. VALIDATE CONSTRAINT takes SHARE UPDATE EXCLUSIVE while
-- existing rows are verified.
ALTER TABLE chat_input_queue VALIDATE CONSTRAINT chat_input_queue_mode_check;
