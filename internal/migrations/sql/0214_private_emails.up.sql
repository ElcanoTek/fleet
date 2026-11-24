-- 0214_private_emails: let a person keep sent email out of the team Outbox.
--
-- sched.tasks.private_emails: a task set to send privately. Every MCP call of
-- its runs carries the request _meta fleet/private_emails=true, and the
-- bundle's email sender then skips the Outbox copy. Default false = sent
-- email is copied as before.
--
-- chat.approvals.private_choice: the Private box of an email approval card as
-- the person approved it. NULL = no choice was sent (the CLI, an older
-- client): the call runs with the model's own private argument. Written in the
-- same UPDATE that claims the card, so the row is never approved without it.
ALTER TABLE sched.tasks ADD COLUMN IF NOT EXISTS private_emails BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE chat.approvals ADD COLUMN IF NOT EXISTS private_choice BOOLEAN;
