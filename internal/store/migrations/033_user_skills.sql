-- 033_user_skills.sql — user-authored Agent Skills (the skills builder,
-- docs/SKILLS.md phase 2).
--
-- A row is one skill a user wrote (or accepted from an agent proposal, later)
-- in the Settings → Skills builder. Unlike bundle/built-in skills — operator
-- content, bind-mounted read-only — a user skill is DB-owned and materialized
-- into the user's own conversation workspaces at turn start, so it is only
-- ever visible to (and runnable by) its author's runs. body is the SKILL.md
-- markdown BODY; the frontmatter is generated from name/description at
-- materialization so the stored fields stay the single source of truth.
--
-- status: 'active' (materialized into runs) | 'disabled' (kept but inert).
-- The set is intentionally open-ended text so a later 'proposed' state
-- (agent-drafted, awaiting review) needs no migration.
CREATE TABLE IF NOT EXISTS user_skills (
    id          TEXT PRIMARY KEY,
    user_email  TEXT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    body        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_skills_user_name ON user_skills (user_email, name);
