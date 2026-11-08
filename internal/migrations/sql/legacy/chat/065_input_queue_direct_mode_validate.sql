-- Second half of 064, in its own transaction so 064's ACCESS EXCLUSIVE lock
-- is released first (the runner applies each file in one transaction, and a
-- lock is held until commit).
--
-- VALIDATE CONSTRAINT takes SHARE UPDATE EXCLUSIVE, so reads and writes keep
-- going while existing rows are checked (every existing row is 'queued' or
-- 'steer', which the widened check accepts).
ALTER TABLE chat_input_queue VALIDATE CONSTRAINT chat_input_queue_mode_check;

-- A first submission carries no conversation id: the server creates the
-- conversation, and a response lost before any header leaves the caller
-- without one. Its resend must still find the input it already accepted, so
-- the key is also looked up per user (LookupInputForUser) before a new
-- conversation is created.
--
-- The build takes a SHARE lock, which blocks writes to the table (not reads)
-- for its duration; CONCURRENTLY cannot run inside the migration
-- transaction. The table is small (terminal rows are purged after 30 days
-- by default), so the build is short.
CREATE INDEX IF NOT EXISTS chat_input_queue_user_key
  ON chat_input_queue (user_email, client_input_id);
