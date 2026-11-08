-- Rollback: restore the 055 index predicate that still admitted 'analyzing'.
-- The status rewrite itself is not reversible — rewritten rows are
-- indistinguishable from rows that were always 'running' — and does not need
-- to be: 'running' is a status every prior binary already understands.
DROP INDEX IF EXISTS idx_tasks_serialization_active;
CREATE INDEX IF NOT EXISTS idx_tasks_serialization_active
    ON tasks (serialization_key)
    WHERE serialization_key IS NOT NULL
    AND status IN ('leased', 'running', 'analyzing');
