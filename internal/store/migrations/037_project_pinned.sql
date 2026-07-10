-- 037_project_pinned.sql — pin a project to the top of the rail's Projects
-- section (#509 follow-up).
--
-- The pin lives on the project row itself, not per-user: only the OWNER can
-- toggle it (the same owner-scoped UPDATE as rename), so for a team-shared
-- project the owner's pin orders the rail for every member. A per-user pin
-- would need a prefs table; deliberately deferred until someone asks.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS pinned BOOLEAN NOT NULL DEFAULT FALSE;
