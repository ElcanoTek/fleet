-- 081_user_skills_visibility.sql — self-service workspace sharing for user
-- skills (docs/SKILLS.md phase 3).
--
-- visibility: 'private' (default; only the author's runs see it) | 'workspace'
-- (when status = 'active', teammates in this deployment also see it in the
-- Workspace pack roster, "/" autocomplete, and turn/scheduled-run skill
-- materialization; bundle/plugin/built-in skills and a teammate's own skill of
-- the same name take precedence). Only the row's owner can edit, unshare, or
-- delete it.
ALTER TABLE user_skills
    ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private';

CREATE INDEX IF NOT EXISTS idx_user_skills_visibility_status
    ON user_skills (visibility, status);
