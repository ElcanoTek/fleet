DROP INDEX IF EXISTS chat.shared_files_one_index_per_folder;
ALTER TABLE chat.shared_files
    DROP COLUMN IF EXISTS folder_index,
    DROP COLUMN IF EXISTS updated_conversation_id,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS previous_sha256,
    DROP COLUMN IF EXISTS previous_content,
    DROP COLUMN IF EXISTS content,
    DROP COLUMN IF EXISTS origin;
