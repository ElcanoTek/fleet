-- 055_unfile_chats_owners_cannot_see.sql — one-time repair of the filing
-- invariant: a chat's project_id must never point at a project its OWNER
-- cannot see (ADR-0057, the "revocation unfiles, never deletes" rule).
--
-- Every path that took a member's access away — unticking "Share with my team"
-- on a project, re-pointing it at a different team, leaving the team, an admin
-- moving someone between teams — cleared the team-share flag and left
-- project_id alone. The rail lists chats through the projects the CALLER can
-- see, so those rows became invisible everywhere: not under Projects, not in
-- Temporary, not in Archived. The chats were never deleted (re-ticking the
-- owner's box brought them straight back), which is what made the failure so
-- unpleasant — a member lost access to conversations THEY own, with no trace
-- and no explanation, for as long as somebody else's setting stayed off.
--
-- The write paths are fixed to unfile in the same transaction as the
-- revocation. This backfill is for the rows they already stranded.
--
-- "Can see" is ListProjectsForUser's rule, restated: the project's owner
-- always, otherwise an exact match between the project's non-empty team_id and
-- the chat owner's users.team_id. Exact, because every team gate in the store
-- is exact — a looser compare here would leave a chat filed in a project the
-- read path still refuses to list.
--
-- An unfiled chat is temporary again: it counts against the unpinned cap and
-- the TTL sweep will eventually reap it unless its owner pins or files it.
-- That is the accepted design, and it is exactly what deleting a project has
-- always done to members' chats — the alternative (leave it filed) is the
-- invisibility this migration exists to end. updated_at is therefore bumped to
-- now, for the same reason DeleteProject bumps it: project_id IS NULL is what
-- re-arms the sweep, so a months-old chat keeping its original timestamp would
-- be reapable at once, with no window in which to pin it. The retention clock
-- starts here instead. The cost is that these chats sort to the top of the
-- owner's Temporary list once, which is arguably the right place for a chat
-- that just reappeared.
--
-- Idempotent: a re-run matches nothing, because every row it touches ends with
-- project_id NULL. team_visible / team_shared_with are cleared only on the
-- rows being unfiled — a chat whose owner still sees its project is not
-- touched at all, so no existing share is revoked by this migration.
UPDATE conversations c
   SET project_id       = NULL,
       team_visible     = FALSE,
       team_shared_with = NULL,
       updated_at       = EXTRACT(EPOCH FROM now())::bigint
 WHERE c.project_id IS NOT NULL
   AND NOT EXISTS (
       SELECT 1
         FROM projects p
        WHERE p.id = c.project_id
          AND (p.owner_email = c.user_email
               OR (p.team_id <> ''
                   AND p.team_id = (SELECT u.team_id FROM users u WHERE u.email = c.user_email)))
   );
