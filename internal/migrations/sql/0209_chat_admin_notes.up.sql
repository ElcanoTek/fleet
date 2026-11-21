-- 0209_chat_admin_notes: a note an administrator pins to a chat (ADR-0117).
--
-- Some people need more help than a single message: an administrator can pin
-- one note to a chat ("I fixed the patrol's schedule; reply here if the 06:30
-- run still fails"). It is shown to the chat's people above the conversation
-- until the administrator replaces or removes it.
--
-- It is a row here and NOT an entry in chat.messages on purpose: the model's
-- context is built from chat.messages, so a note kept outside it can never
-- reach the model, be summarised by a compaction, or be copied by a branch.
-- One note per chat (the primary key); it goes with the chat when the chat is
-- deleted for good.
--
-- seen_at / seen_by record the first person in the chat who pressed "Got it",
-- so the administrator knows the note was read. Replacing the note's text
-- clears them.
--
-- version names one text of one note. A reader acknowledges the version it
-- was shown and an administrator edits against the version they opened, so a
-- version must never be used twice, including by a note pinned after the one
-- before it was removed. It therefore comes from a sequence, not from a
-- timestamp: updated_at is in seconds, and two texts can be written in one.
CREATE SEQUENCE IF NOT EXISTS chat.admin_note_version_seq;

CREATE TABLE IF NOT EXISTS chat.admin_notes (
    conversation_id TEXT   PRIMARY KEY REFERENCES chat.conversations(id) ON DELETE CASCADE,
    body            TEXT   NOT NULL,
    author          TEXT   NOT NULL,
    via             TEXT   NOT NULL CHECK (via IN ('cli', 'web')),
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    version         BIGINT NOT NULL DEFAULT nextval('chat.admin_note_version_seq'),
    seen_at         BIGINT,
    seen_by         TEXT   NOT NULL DEFAULT ''
);
