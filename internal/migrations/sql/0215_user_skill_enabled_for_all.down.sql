-- Undoes 0215. The previous release neither reads nor writes the column: every
-- skill goes back to reaching only its owner's turns, and teammates can still
-- add a workspace-shared one to their own skills. Safe to run again.
ALTER TABLE chat.user_skills DROP COLUMN IF EXISTS enabled_for_all;
