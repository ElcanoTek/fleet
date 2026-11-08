-- 026_typed_memories.sql — typed, provenanced user memory (#515 MVP).
--
-- Upgrades the flat fact strings of 005/007 into typed memory records with
-- bi-temporal timestamps and soft retirement, per the issue's staged triage
-- (MVP: typed records + provenance + manual edit/pin/forget; contradiction
-- candidates come next; the full entity/relation graph is the deferred north
-- star). All columns are additive with backfills, so existing memories are
-- byte-for-byte preserved as kind='fact'.
--
-- Two DISTINCT time axes (do not conflate — the classic bi-temporal mistake):
--   valid_from / valid_to  — VALID time: when the fact is true in the world
--                            (user-editable; NULL = open-ended/unknown).
--   learned_at             — TRANSACTION time: when fleet recorded the fact
--                            (backfilled from created_at; never edited).
--   retired_at             — TRANSACTION time: when fleet stopped injecting it
--                            (soft retirement — an audit event, NOT valid_to).
-- retired_by links to the memory that superseded this one (set by the
-- stage-2 contradiction flow; NULL for manual retirement).
ALTER TABLE memories ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'fact';
ALTER TABLE memories ADD COLUMN IF NOT EXISTS pinned BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_from BIGINT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS valid_to BIGINT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS learned_at BIGINT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS retired_at BIGINT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS retired_by TEXT;

UPDATE memories SET learned_at = created_at WHERE learned_at IS NULL;

-- Backs the injection read: active (non-retired) memories, pinned first,
-- freshest next.
CREATE INDEX IF NOT EXISTS idx_memories_user_active
    ON memories (user_email, pinned DESC, updated_at DESC)
    WHERE retired_at IS NULL;
