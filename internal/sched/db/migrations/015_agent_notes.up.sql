-- Published/curated knowledge base, injected into every run's system prompt.
CREATE TABLE IF NOT EXISTS agent_notes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,             -- ^[a-z0-9_-]{1,128}$, immutable
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,                    -- markdown, <= 1 MiB
    status      TEXT NOT NULL DEFAULT 'published',-- 'published' | 'archived' (soft-delete)
    created_by  TEXT NOT NULL,                    -- admin email (or run label for imports)
    updated_by  TEXT NOT NULL,
    created_at  BIGINT NOT NULL,                  -- unix seconds (matches chat memories convention)
    updated_at  BIGINT NOT NULL,
    version     INT NOT NULL DEFAULT 1            -- bumped on each publish
);

CREATE INDEX IF NOT EXISTS idx_agent_notes_status ON agent_notes(status);
CREATE INDEX IF NOT EXISTS idx_agent_notes_updated_at ON agent_notes(updated_at DESC);

-- Agent-proposed edits awaiting admin curation. Never auto-applied.
CREATE TABLE IF NOT EXISTS agent_note_proposals (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    note_id       UUID REFERENCES agent_notes(id) ON DELETE SET NULL, -- NULL = create-new
    slug          TEXT NOT NULL,                  -- target slug (existing or new)
    title         TEXT NOT NULL,
    body          TEXT NOT NULL,                  -- proposed markdown
    reason        TEXT,                           -- agent's free-text justification
    status        TEXT NOT NULL DEFAULT 'pending',-- 'pending' | 'published' | 'rejected'
    proposed_by   TEXT NOT NULL,                  -- run label: scheduled task id or chat user email
    proposed_at   BIGINT NOT NULL,
    decided_by    TEXT,                           -- admin email
    decided_at    BIGINT,
    decision_note TEXT                            -- rejection reason / publish note
);

CREATE INDEX IF NOT EXISTS idx_agent_note_proposals_pending
    ON agent_note_proposals(status) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_agent_note_proposals_slug
    ON agent_note_proposals(slug);
