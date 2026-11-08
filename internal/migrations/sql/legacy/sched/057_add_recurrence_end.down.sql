-- 057_add_recurrence_end.down.sql — drop the recurrence end conditions.
ALTER TABLE tasks DROP COLUMN IF EXISTS recurrence_remaining;
ALTER TABLE tasks DROP COLUMN IF EXISTS recurrence_until;
