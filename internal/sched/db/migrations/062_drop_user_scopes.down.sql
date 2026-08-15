-- Rollback: re-add users.scopes as 001_initial_schema created it. Like the other
-- destructive down migrations here this restores the STRUCTURE only; the stored
-- patterns are gone. No code reads the column on either side of the rollback.
ALTER TABLE users ADD COLUMN IF NOT EXISTS scopes JSONB DEFAULT '[]';
