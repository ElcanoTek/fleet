-- 073_add_recurrence_parked_reason.down.sql — drop the park reason.
ALTER TABLE tasks DROP COLUMN IF EXISTS recurrence_parked_reason;
