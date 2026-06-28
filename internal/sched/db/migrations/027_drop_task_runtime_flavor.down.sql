-- Rollback: re-add the per-task runtime-flavor column (nullable, as 018 created it).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS runtime_flavor TEXT;
