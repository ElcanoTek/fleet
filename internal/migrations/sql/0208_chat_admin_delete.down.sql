DELETE FROM chat.admin_requests WHERE kind IN ('delete', 'restore', 'purge');
ALTER TABLE chat.admin_requests DROP CONSTRAINT IF EXISTS admin_requests_kind_check;
ALTER TABLE chat.admin_requests
  ADD CONSTRAINT admin_requests_kind_check
  CHECK (kind IN ('message', 'stop', 'transfer', 'archive'));
