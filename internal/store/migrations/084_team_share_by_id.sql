-- 084_team_share_by_id.sql — a team share names its audience by a stable team
-- ID, not by the team's current label (docs/TEAM-SHARING.md "Audience
-- identity", ADR-0093).
--
-- Migration 054 stamped the audience as conversations.team_shared_with, the
-- team NAME at share time, and every read gate compared the caller's
-- users.team_id against that string. A team's name is not its identity:
-- RenameTeam relabelled users and projects but not the stamps, so a shared
-- chat kept pointing at the OLD name after a rename, and anyone who later
-- formed a team under that freed name matched the stamp and could read every
-- chat (and its shared outputs) the original team had been given.
--
-- The fix gives each team that has ever been shared with a row in `teams`
-- with a surrogate id, stamps that id on the share, and makes the read gates
-- require it. A rename relabels the row (the id, and so every share, follows
-- the team); a merge re-points the shares to the surviving team's id; a name
-- that nobody holds any more is retired (its shares are unshared and its row
-- deleted) before anyone can join it, so a new team under an old name starts
-- with a new id and inherits nothing.
--
-- name is UNIQUE and compared exactly, like every other team gate.
CREATE TABLE IF NOT EXISTS teams (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at BIGINT NOT NULL
);

-- Nullable with no default: a metadata-only change. ON DELETE SET NULL is a
-- backstop only — the store unshares a retired team's chats before it deletes
-- the row.
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS team_shared_with_id BIGINT
    REFERENCES teams(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_conversations_team_shared_with_id
    ON conversations (team_shared_with_id)
    WHERE team_shared_with_id IS NOT NULL;

-- A share whose stamp no longer names its owner's CURRENT team cannot be a
-- live share: sharing requires the owner's team, and leaving or changing team
-- unshares (unshareOnTeamChangeTx). The only way to get here is a rename that
-- left the stamp behind — exactly the orphan this migration exists to close —
-- or pre-054 drift. Which team such a stamp "really" meant cannot be told
-- apart from the data, so it is unshared rather than guessed at; the owner can
-- share it again in one click and it then carries an id.
UPDATE conversations c
   SET team_visible = FALSE, team_shared_with = NULL
 WHERE c.team_shared_with IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM users u
                    WHERE u.email = c.user_email
                      AND u.team_id = c.team_shared_with);

-- Every surviving stamp names a team its owner is in now: give that team a row
-- and stamp the id.
INSERT INTO teams (name, created_at)
SELECT DISTINCT c.team_shared_with, EXTRACT(EPOCH FROM now())::BIGINT
  FROM conversations c
 WHERE c.team_shared_with IS NOT NULL
ON CONFLICT (name) DO NOTHING;

UPDATE conversations c
   SET team_shared_with_id = t.id
  FROM teams t
 WHERE t.name = c.team_shared_with
   AND c.team_shared_with_id IS NULL;

-- Same normalization as 054: a row still flagged visible with no audience is
-- not shared with anyone.
UPDATE conversations
   SET team_visible = FALSE
 WHERE team_visible = TRUE
   AND (team_shared_with IS NULL OR team_shared_with_id IS NULL);
