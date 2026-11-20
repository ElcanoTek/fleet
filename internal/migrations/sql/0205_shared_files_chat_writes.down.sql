-- Chat-written rows keep their only copy in `content`; the previous server
-- version would read them as admin uploads with no canonical bytes. Remove them
-- (and their staged copies, which the reconciler then sweeps as strays) before
-- the columns go.
DELETE FROM chat.shared_files WHERE origin = 'chat';
DROP INDEX IF EXISTS chat.shared_files_one_index_per_folder;
ALTER TABLE chat.shared_files
    DROP COLUMN IF EXISTS folder_index,
    DROP COLUMN IF EXISTS updated_conversation_id,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS previous_sha256,
    DROP COLUMN IF EXISTS previous_content,
    DROP COLUMN IF EXISTS content,
    DROP COLUMN IF EXISTS origin;
