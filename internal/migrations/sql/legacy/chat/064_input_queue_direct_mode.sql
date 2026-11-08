-- Idempotency for directly started turns (#984 follow-through).
--
-- A caller's input_id (client_input_id) was durable only on the queue path: a
-- submission that arrived while a turn was busy became a queue row and a
-- re-POST of the same key returned that row. A submission that started a turn
-- DIRECTLY recorded no key, so when its stream was lost and the caller resent
-- the same input_id, the server could not recognise the input it had already
-- accepted and ran it a second time — model spend and tool side effects
-- included.
--
-- A direct turn now claims its key with a row of mode 'direct' in the same
-- table, so one unique index (chat_input_queue_idem) covers both paths and a
-- key is accepted exactly once whichever path took it. Direct rows are never
-- queue items: the queue listing, the drain claim, Stop sweeps, remove and
-- promote all skip them, and boot recovery settles them (completed when the
-- turn's user entry committed, else cancelled) rather than re-queueing them.
--
-- The CHECK is replaced NOT VALID, which needs no scan, so the ACCESS
-- EXCLUSIVE lock this statement takes is held only for the swap. Migrations
-- run one file per transaction and a lock is held until commit, so the
-- validation and the new index live in 065, their own transaction, rather
-- than here under this lock.
ALTER TABLE chat_input_queue DROP CONSTRAINT IF EXISTS chat_input_queue_mode_check;
ALTER TABLE chat_input_queue
  ADD CONSTRAINT chat_input_queue_mode_check CHECK (mode IN ('queued', 'steer', 'direct')) NOT VALID;
