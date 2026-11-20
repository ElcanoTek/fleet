-- 0205_shared_files_chat_writes: shared files a chat writes (ADR-0114,
-- docs/SHARED-FILES.md "Files chats write").
--
-- Until now every row in chat.shared_files was an admin upload whose bytes live
-- in <DataDir>/shared_files/<id>. The shared_file_write tool lets a chat (or a
-- scheduled run) create a TEXT file in the same library, append to it, or
-- replace it, so one conversation's record (an optimization log, a running
-- register) is readable from every other conversation on the server.
--
-- A chat-written file keeps its text HERE rather than in the data dir: the
-- nightly backup is a pg_dump, so a record the team relies on must live in
-- Postgres to survive a lost disk, and an append is then one row-locked
-- UPDATE that two writers (or two replicas) cannot interleave.
--
--   origin               'admin' (an upload; bytes in the data dir, content
--                        NULL) or 'chat' (written by shared_file_write; text in
--                        content). Chats may write only 'chat' rows.
--   content              the file's text for a 'chat' row; NULL for 'admin'.
--   previous_content     the text a replace or undo displaced, so one undo can
--                        put it back. NULL when there is nothing to restore.
--   previous_sha256      sha256 of previous_content ('' when NULL).
--   updated_by           email of whoever last wrote the file ('' for uploads).
--   updated_conversation_id  the chat (or "task:<id>") that last wrote it.
--   folder_index         this file is its folder's entry point: a folder with
--                        many files is announced to the model as one line that
--                        names this file. At most one per folder (index below).
--
-- Every new column has a constant default, so the ADD is a catalog-only change
-- (no table rewrite) and every existing row reads as an admin upload.
ALTER TABLE chat.shared_files
    ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'admin'
        CONSTRAINT shared_files_origin_check CHECK (origin IN ('admin', 'chat')),
    ADD COLUMN IF NOT EXISTS content TEXT,
    ADD COLUMN IF NOT EXISTS previous_content TEXT,
    ADD COLUMN IF NOT EXISTS previous_sha256 TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS updated_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS updated_conversation_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS folder_index BOOLEAN NOT NULL DEFAULT FALSE;

-- One index file per folder. The library root is the folder '' and may have
-- one too, though the prompt block only collapses named folders.
CREATE UNIQUE INDEX IF NOT EXISTS shared_files_one_index_per_folder
    ON chat.shared_files (folder) WHERE folder_index;
