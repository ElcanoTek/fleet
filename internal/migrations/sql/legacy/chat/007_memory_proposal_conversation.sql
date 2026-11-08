-- 007_memory_proposal_conversation.sql — scope memory proposals to a conversation.
--
-- Pending proposals (source='proposed') need a conversation_id so the UI can
-- re-hydrate the Save/Don't-Save card on conversation load — without it,
-- visibilitychange/focus events trigger a loadConversation that wipes the
-- transient client-side proposal state and the user loses the prompt. Set
-- conversation_id on insert; cleared back to NULL when the proposal is
-- accepted (becomes a global memory) or deleted (rejected).

ALTER TABLE memories ADD COLUMN IF NOT EXISTS conversation_id TEXT;

-- Pending proposals scoped by (user, conversation). Partial index so it stays
-- tight: accepted/manual memories don't need to be in here.
CREATE INDEX IF NOT EXISTS idx_memories_pending_proposals
    ON memories(user_email, conversation_id)
    WHERE source = 'proposed';
