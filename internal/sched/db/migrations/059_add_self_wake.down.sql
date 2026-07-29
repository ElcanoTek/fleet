-- 059_add_self_wake.down.sql — drop the self-wake state.
DROP INDEX IF EXISTS idx_tasks_wake_due;
ALTER TABLE tasks DROP COLUMN IF EXISTS wake_cycles;
ALTER TABLE tasks DROP COLUMN IF EXISTS wake_reason;
ALTER TABLE tasks DROP COLUMN IF EXISTS wake_note;
ALTER TABLE tasks DROP COLUMN IF EXISTS wake_event_key;
ALTER TABLE tasks DROP COLUMN IF EXISTS wake_at;
