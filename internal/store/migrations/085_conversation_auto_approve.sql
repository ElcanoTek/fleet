-- 084_conversation_auto_approve.sql — per-conversation auto-approve toggle.
--
-- When auto_approve is true on a conversation, session-approvable critical
-- tools run automatically in that conversation and record an action card
-- instead of blocking on a countdown. Tools listed in
-- agent_policy.critical_tool_no_session_approval, outbound email, and
-- handler-only task cards still stage their own card.
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS auto_approve BOOLEAN NOT NULL DEFAULT FALSE;
