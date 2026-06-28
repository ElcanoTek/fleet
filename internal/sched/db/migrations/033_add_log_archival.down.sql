-- Reverse log archival (#272). Rows whose payload was archived have session_data
-- NULL and their bytes only in session_data_gz (which this migration drops), so
-- they cannot satisfy the restored NOT NULL and are unrecoverable on rollback —
-- delete them first, then drop the archival columns and restore the constraint.
DELETE FROM logs WHERE session_data IS NULL;
ALTER TABLE logs DROP COLUMN IF EXISTS session_compression;
ALTER TABLE logs DROP COLUMN IF EXISTS session_data_gz;
ALTER TABLE logs ALTER COLUMN session_data SET NOT NULL;
