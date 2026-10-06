-- 070_team_shared_files.sql — a team share carries the chat's outputs
-- (ADR-0079, docs/TEAM-SHARING.md "Files in a team share").
--
-- Three new tables, all additive (no existing table is altered, so nothing is
-- rewritten and no older binary reads anything it does not know about).

-- Per-file share state, stored as EXCLUSIONS. An output (a workspace file the
-- agent presented in a reply) is shared with the chat by default; a row here
-- is the owner unchecking one. Exclusions are deliberately independent of
-- conversations.team_visible: they survive stop sharing, sharing again,
-- archiving and unarchiving, so "I unchecked the confidential file" is never
-- silently undone by a later fast-path share. They die with the chat.
CREATE TABLE IF NOT EXISTS conversation_output_exclusions (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    path            TEXT NOT NULL,
    created_at      BIGINT NOT NULL,
    PRIMARY KEY (conversation_id, path)
);

-- Where a TEAMMATE's branch came from, and which shared files were copied into
-- it. Only a cross-user branch gets a row: the owner's own branch copies their
-- history in full and needs no record. source_conversation_id has no foreign
-- key on purpose — a branch outlives its source being deleted, and the record
-- of where it came from must outlive it too.
--
-- source_max_message_id is the source's high-water mark at branch time (its
-- MAX(messages.id) then): "has added messages since you branched" is any
-- source message with a larger id. An id, not branched_at against
-- messages.created_at, because both are whole seconds — a message written in
-- the same second as the branch would otherwise never count.
--
-- files_announced is the one-shot latch for the injected first-turn note that
-- tells the agent which files it actually has (the transcript can mention
-- files that were withheld).
CREATE TABLE IF NOT EXISTS conversation_branch_origins (
    conversation_id        TEXT PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    source_conversation_id TEXT NOT NULL,
    source_owner_email     TEXT NOT NULL,
    -- The source's title AS THE BRANCHER SAW IT. A snapshot, never re-read:
    -- once the source is unshared, its later renames are not theirs to see.
    source_title           TEXT NOT NULL DEFAULT '',
    branched_at            BIGINT NOT NULL,
    source_max_message_id  BIGINT NOT NULL DEFAULT 0,
    copied_files           JSONB NOT NULL DEFAULT '[]'::jsonb,
    withheld_files         JSONB NOT NULL DEFAULT '[]'::jsonb,
    files_announced        BOOLEAN NOT NULL DEFAULT FALSE
);
-- "Has this viewer branched that chat?" is asked by source id.
CREATE INDEX IF NOT EXISTS idx_branch_origins_source
    ON conversation_branch_origins (source_conversation_id);

-- Per-person, per-project UI state that must follow a user across devices:
-- the getting-started card ("Keep personal", and whether they have shared a
-- chat here yet) and which Sources groups they left open.
CREATE TABLE IF NOT EXISTS project_user_state (
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_email      TEXT NOT NULL,
    kept_personal   BOOLEAN NOT NULL DEFAULT FALSE,
    has_shared_chat BOOLEAN NOT NULL DEFAULT FALSE,
    sources_open    JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at      BIGINT NOT NULL,
    PRIMARY KEY (project_id, user_email)
);
