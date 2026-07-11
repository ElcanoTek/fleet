-- 055_add_task_serialization_key.up.sql — opaque per-task mutual-exclusion key (#709).
--
-- The intake caller may tag tasks with an opaque serialization_key; fleet
-- guarantees at most one task per key is in an ACTIVE state
-- (leased/running/analyzing) at a time. A pending task whose key matches an
-- active task is not claimable and waits for a later claim pass. Fleet never
-- interprets the key's contents — its meaning is owned by the intake side
-- (coupling doctrine; moc#442/#448 parity). NULL = unserialized (the default,
-- byte-identical behavior for every existing task and caller).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS serialization_key TEXT;

-- Partial index for the "is another task with this key active?" existence
-- checks: the advisory-lock re-check at claim time and the best-effort
-- visibility filter in the claim candidate query (see db.ClaimNextPendingTask).
CREATE INDEX IF NOT EXISTS idx_tasks_serialization_active
    ON tasks (serialization_key)
    WHERE serialization_key IS NOT NULL
    AND status IN ('leased', 'running', 'analyzing');
