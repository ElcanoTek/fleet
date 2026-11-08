-- Per-conversation lockdown flag. Set at conversation creation; never
-- mutated thereafter. When true, the agent loop runs the conversation
-- in a forced-container sandbox and rejects model slugs outside the
-- operator's allow-list (CHAT_LOCKDOWN_ALLOWED_MODELS). Drives the
-- "Lockdown chat" UX on the frontend.

ALTER TABLE conversations
    ADD COLUMN lockdown BOOLEAN NOT NULL DEFAULT FALSE;
