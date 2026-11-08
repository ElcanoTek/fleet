-- Task name (#238): an optional, human-readable label for a scheduled task. It is
-- the identity key used by the task-definition import/export endpoints
-- (GET /tasks/export, POST /tasks/import) for conflict detection on import
-- (conflict=skip|replace|error). Empty/NULL means "unnamed"; such tasks are
-- always created fresh on import (no name-based collision is possible).
--
-- The unique index is PARTIAL on a non-empty name so that:
--   - any number of tasks may omit a name (NULL or ''), and
--   - two tasks may not share the same non-empty name.
-- The COALESCE normalizes a NULL column read back to '' for the Go *string path.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';

-- Partial unique index: only non-empty names are enforced unique. NULLs and ''
-- are excluded so unnamed tasks never collide.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_name_unique
    ON tasks (name)
    WHERE name IS NOT NULL AND name <> '';
