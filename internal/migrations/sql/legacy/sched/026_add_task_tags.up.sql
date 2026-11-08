-- Task tagging (#212): user-defined labels for organizing and filtering
-- scheduled tasks. Stored as a JSONB array of strings, mirroring the existing
-- `files` column (migration 001 + the GIN index in 003) so the codebase's
-- string-slice convention and pgx scanning path are reused unchanged. The GIN
-- index backs `tags @> '["x"]'` containment queries (AND-filtering by tag).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS tags JSONB NOT NULL DEFAULT '[]';

CREATE INDEX IF NOT EXISTS idx_tasks_tags ON tasks USING GIN (tags);
