-- 069_add_lineage_id.down.sql — drop the job lineage key.
ALTER TABLE tasks DROP COLUMN IF EXISTS lineage_id;
