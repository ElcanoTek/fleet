-- Sub-second acceptance instant for the Stop scope=all sweep (#1477).
-- created_at is whole seconds, so a Stop could not tell a row accepted just
-- BEFORE it from one accepted just AFTER it in the same second: the sweep
-- cancelled every still-queued row, and the claim-limbo epoch gate had to be
-- strict to let a fresh post-Stop submission run. accepted_at_ns records the
-- fleet process's wall clock in nanoseconds at EnqueueInput, the same clock
-- the Stop instant is taken from, so the sweep cancels exactly the rows that
-- existed when Stop began and the gate refuses exactly the same set.
-- Nullable: rows written by an older binary mid-deploy have no value, and
-- readers fall back to created_at * 1e9 for them (backfilled here for every
-- existing row).
ALTER TABLE chat_input_queue ADD COLUMN accepted_at_ns BIGINT;
UPDATE chat_input_queue SET accepted_at_ns = created_at * 1000000000 WHERE accepted_at_ns IS NULL;
