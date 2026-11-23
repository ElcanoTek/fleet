-- 0211_message_uploads: which uploaded files each message refers to.
--
-- An image a person attaches in a chat stays at
-- <EMAIL_ATTACHMENT_DIR>/uploads/<user>/<token>/<name>; the user message
-- records its path (content.images[].path) and every later turn reads the file
-- again so the model keeps seeing it. The hourly attachment cleanup used to
-- remove every file under that tree once it was older than the conversation
-- window, whether or not a chat that is still kept refers to it, so a pinned,
-- archived, project or shared chat — or a temporary chat still in use —
-- silently lost its images after the window.
--
-- One row per (message, upload). The cleanup keeps an upload while a row names
-- it (store.AttachmentKeeper), so an upload stays exactly as long as a message
-- that refers to it does: a message goes with its conversation (ON DELETE
-- CASCADE on chat.messages), and with it the row, so whatever removes a
-- conversation — the retention sweep, the per-user cap, a delete, the
-- soft-delete purge, account erasure — also releases its uploads, with no
-- second copy of the retention rule. The store writes the rows in the same
-- transaction as the messages (store.insertMessageUploads).
CREATE TABLE IF NOT EXISTS chat.message_uploads (
    message_id  BIGINT NOT NULL REFERENCES chat.messages(id) ON DELETE CASCADE,
    upload_path TEXT   NOT NULL,
    PRIMARY KEY (message_id, upload_path)
);

-- The cleanup looks an upload up by its path.
CREATE INDEX IF NOT EXISTS message_uploads_path_idx
    ON chat.message_uploads (upload_path);

-- Messages written before this migration are recorded by the server after it
-- starts, in batches it can resume (store.BackfillMessageUploads), not here: a
-- scan of every message inside this transaction would hold up the boot and,
-- on a large database, outlast the service's start timeout and start over on
-- every restart. Until the backfill sets completed_at, the cleanup keeps every
-- upload. through_id is how far it got; upto_id is the last message it must
-- read: the newest message now, raised to the newest one each time a server
-- starts the backfill, because any command of this release migrates the
-- database and a server of the previous release may write messages until it
-- is restarted. The first server of this release marks it complete, even on
-- an empty database.
CREATE TABLE IF NOT EXISTS chat.message_uploads_backfill (
    id           SMALLINT PRIMARY KEY CONSTRAINT message_uploads_backfill_one_row CHECK (id = 1),
    through_id   BIGINT NOT NULL DEFAULT 0,
    upto_id      BIGINT NOT NULL,
    completed_at BIGINT
);

INSERT INTO chat.message_uploads_backfill (id, through_id, upto_id)
SELECT 1, 0, COALESCE(MAX(id), 0) FROM chat.messages
ON CONFLICT (id) DO NOTHING;
