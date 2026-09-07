-- 068_add_previous_occurrence_id.down.sql — drop the recurrence lineage pointer.
ALTER TABLE tasks DROP COLUMN IF EXISTS previous_occurrence_id;
