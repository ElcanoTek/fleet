-- 081_user_skills_visibility.sql — self-service workspace sharing for user
-- skills (docs/SKILLS.md phase 2).
--
-- visibility: 'private' (default; only the author's runs see it) | 'workspace'
-- (when status = 'active', published to GET /skills under shared_skills and
-- surfaced in Settings → Skills → Workspace pack so teammates can inspect and
-- click "Add to my skills" to create an active private copy in their own
-- user_skills roster). Only the row's owner can edit, unshare, or delete it.
ALTER TABLE user_skills
    ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'private';

CREATE INDEX IF NOT EXISTS idx_user_skills_visibility_status
    ON user_skills (visibility, status);
