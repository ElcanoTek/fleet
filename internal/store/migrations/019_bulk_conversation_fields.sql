-- 019_bulk_conversation_fields.sql — bulk conversation operations (#279).
--
-- folder     : a free-form bucket name (e.g. "Archive", "Old work"). Empty
--              string is the default "no folder" state, so existing rows are
--              unaffected and the sidebar renders them as before.
-- labels     : a tag set (text[]). Empty array is the default.
-- deleted_at : nullable soft-delete tombstone (#279). NULL = live; non-NULL =
--              soft-deleted (hidden from GET /conversations and search). Only
--              written when FLEET_CONVERSATION_SOFT_DELETE=true; the default
--              (unset) hard-delete path never touches it. A 30-day sweeper
--              permanently removes rows whose deleted_at falls out of window.
ALTER TABLE conversations
    ADD COLUMN folder     TEXT      NOT NULL DEFAULT '',
    ADD COLUMN labels     TEXT[]    NOT NULL DEFAULT '{}',
    ADD COLUMN deleted_at TIMESTAMPTZ;

-- Supports ?folder= and ?label= filtered bulk delete and the soft-delete sweep.
CREATE INDEX idx_conv_user_folder   ON conversations (user_email, folder);
CREATE INDEX idx_conv_deleted_at    ON conversations (deleted_at) WHERE deleted_at IS NOT NULL;
