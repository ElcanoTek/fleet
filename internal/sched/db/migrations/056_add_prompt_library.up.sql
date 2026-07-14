-- Hybrid prompt library: bundle/Git prompts are read-only filesystem content;
-- this table stores approachable UI-authored prompts. Private rows are visible
-- only to their owner; workspace rows are readable by every authenticated user.
CREATE TABLE IF NOT EXISTS prompt_library (
    id UUID PRIMARY KEY,
    owner_username TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    visibility TEXT NOT NULL DEFAULT 'private'
        CHECK (visibility IN ('private', 'workspace')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (char_length(name) BETWEEN 1 AND 120),
    CHECK (octet_length(content) BETWEEN 1 AND 262144)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_prompt_library_owner_name
    ON prompt_library (owner_username, LOWER(name));
CREATE INDEX IF NOT EXISTS idx_prompt_library_visibility_owner
    ON prompt_library (visibility, owner_username, updated_at DESC);
