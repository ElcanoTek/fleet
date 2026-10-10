-- 072_approval_resume_state.sql — resume after approval
-- (agent_policy.critical_tool_resume, docs/RESUME-AFTER-APPROVAL.md).
--
-- A card staged for a tool the bundle opted in is ARMED when it is created
-- ('armed'). Once the card reaches a terminal outcome the chat server claims
-- every armed, settled card of the conversation in one transaction
-- ('claimed') and enqueues the one turn that continues the task, or records
-- why it did not ('skipped'). A card superseded by a newer call of the same
-- tool is disarmed ('superseded'); a settled card whose resume was never
-- started when the process stopped is 'dropped' at the next boot, with a note
-- in the conversation. '' is every other row: a tool that did not opt in, and
-- every row written before this column existed, so the constant default is
-- exactly the old behaviour (a metadata-only ADD on Postgres 11+, no rewrite).
ALTER TABLE approvals ADD COLUMN resume_state TEXT NOT NULL DEFAULT '';

-- The claim and the boot sweep look for armed rows only; a partial index keeps
-- that lookup off the (much larger) set of rows that never opted in.
CREATE INDEX idx_approvals_resume_armed ON approvals (conversation_id) WHERE resume_state = 'armed';
