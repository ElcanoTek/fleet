-- 054_conversation_team_shared_with.sql — record WHICH team a chat was shared
-- with, instead of inferring it (ADR-0057).
--
-- `conversations.team_visible` (ADR-0013) is a bare boolean. Every read that
-- honored it inferred the audience from the OWNER'S CURRENT TEAM — "list the
-- team-visible chats of users whose users.team_id matches mine". That inference
-- is wrong the moment the owner's team changes: an admin moving Bob from
-- `quant` to `ops` silently re-points every chat Bob shared with quant at ops,
-- and an ops member who never had anything to do with quant can then list and
-- read them. Nothing revoked Bob's opt-in, and nothing asked him about the new
-- audience.
--
-- The flag was unreachable before now (no UI ever set it), so this was latent;
-- exposing team sharing is what makes it live. The fix is to stop inferring:
-- the share names its own audience, stamped when the owner opts in and cleared
-- when the pairing breaks. Reads compare the CALLER'S team against this column,
-- so moving the owner cannot change who can see what.
--
-- NULL = not shared with any team (the only state for a chat whose
-- team_visible is FALSE). Nullable with no default, so this is a metadata-only
-- change: no table rewrite, no ACCESS EXCLUSIVE lock beyond the catalog update.
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS team_shared_with TEXT;

-- The read gates filter on this column; the partial index keeps them off a
-- sequential scan while staying tiny (only shared rows are indexed).
CREATE INDEX IF NOT EXISTS idx_conversations_team_shared_with
    ON conversations (team_shared_with)
    WHERE team_shared_with IS NOT NULL;

-- Backfill any row already opted in, using the audience that was in effect
-- under the old inference (the owner's current team). In practice this touches
-- nothing — no shipped UI ever set team_visible — but a box whose operator
-- drove the ADR-0013 API directly keeps exactly the visibility it had, rather
-- than silently losing or widening it.
UPDATE conversations c
   SET team_shared_with = u.team_id
  FROM users u
 WHERE u.email = c.user_email
   AND c.team_visible = TRUE
   AND c.team_shared_with IS NULL
   AND COALESCE(u.team_id, '') <> '';

-- A row still flagged visible with no resolvable audience (its owner has no
-- team) is not shared with anyone. Make that explicit rather than leaving a
-- TRUE flag that every read must special-case.
UPDATE conversations
   SET team_visible = FALSE
 WHERE team_visible = TRUE
   AND team_shared_with IS NULL;
