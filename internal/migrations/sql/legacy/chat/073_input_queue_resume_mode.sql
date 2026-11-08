-- Resume after approval (docs/RESUME-AFTER-APPROVAL.md): the turn fleet
-- starts after a settled approval card is an input-queue row of mode
-- 'resume', so it reuses the queue's durability, FIFO drain, Stop sweep and
-- remove/send-now exactly like a queued follow-up ('queued'), and is drained
-- through the same launch path (startTurn). Every queue query that filters on
-- mode excludes only 'direct', so a 'resume' row needs no other change.
--
-- Same two-step swap as 064/065: the CHECK is replaced NOT VALID here (no
-- scan, the ACCESS EXCLUSIVE lock is held only for the swap) and validated in
-- 074, its own transaction.
ALTER TABLE chat_input_queue DROP CONSTRAINT IF EXISTS chat_input_queue_mode_check;
ALTER TABLE chat_input_queue
  ADD CONSTRAINT chat_input_queue_mode_check CHECK (mode IN ('queued', 'steer', 'direct', 'resume')) NOT VALID;
