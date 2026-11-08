DROP INDEX IF EXISTS idx_tasks_lease_expires_at;
ALTER TABLE tasks DROP COLUMN IF EXISTS lease_expires_at;
ALTER TABLE tasks DROP COLUMN IF EXISTS lease_owner;
