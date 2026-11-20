-- 0208_chat_admin_delete: administrators can delete chats (ADR-0116).
--
-- Three more admin request kinds, carried out by the running server like a
-- stop or a transfer (chat.admin_requests, migration 0207):
--   delete   moves a chat to the recycle bin (deleted_at = now()): it leaves
--            every list and stops, and the 30-day tombstone purge removes it
--            unless it is restored first;
--   restore  takes a chat back out of the recycle bin;
--   purge    deletes a chat permanently at once (an emergency, e.g. a full
--            disk), its workspace reclaimed by the orphan sweep.
ALTER TABLE chat.admin_requests DROP CONSTRAINT IF EXISTS admin_requests_kind_check;
ALTER TABLE chat.admin_requests
  ADD CONSTRAINT admin_requests_kind_check
  CHECK (kind IN ('message', 'stop', 'transfer', 'archive', 'delete', 'restore', 'purge'));
