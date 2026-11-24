-- Undoes 0214. The previous release neither reads nor writes either column.
-- Safe to run again.
ALTER TABLE chat.approvals DROP COLUMN IF EXISTS private_choice;
ALTER TABLE sched.tasks DROP COLUMN IF EXISTS private_emails;
