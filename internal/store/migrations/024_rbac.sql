-- Team RBAC (#237): roles + team-scoped, opt-in conversation visibility.
--
-- role:    'member' (default — sends messages, sees only its own data),
--          'viewer' (read-only: may NOT send/create/mutate, but may read its own
--                    data and team-shared conversations), or
--          'admin'  (full access; complements the ADMIN_EMAILS env allowlist,
--                    which keeps working as an out-of-band bootstrap gate).
-- team_id: nullable free-text group label. Members sharing a team_id form a
--          trust group; a conversation becomes visible to that group ONLY when
--          its owner opts in via team_visible (below) — team membership alone
--          never auto-exposes private conversations.
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS role    TEXT NOT NULL DEFAULT 'member'
             CHECK (role IN ('member', 'viewer', 'admin')),
  ADD COLUMN IF NOT EXISTS team_id TEXT;

CREATE INDEX IF NOT EXISTS idx_users_team ON users(team_id) WHERE team_id IS NOT NULL;

-- team_visible: the owner of a conversation opts it into team visibility
-- (POST /conversations/{id}/share-with-team). Default FALSE keeps every existing
-- and new conversation private until its owner explicitly shares it — so adding a
-- team_id can never retroactively expose someone's history.
ALTER TABLE conversations
  ADD COLUMN IF NOT EXISTS team_visible BOOLEAN NOT NULL DEFAULT FALSE;
