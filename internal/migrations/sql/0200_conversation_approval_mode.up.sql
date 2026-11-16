-- 0200_conversation_approval_mode: three-level per-conversation approval mode
-- ('manual', 'auto', 'autonomous'). Keeps auto_approve boolean in lockstep
-- (true for 'auto' and 'autonomous', false for 'manual').

ALTER TABLE chat.conversations
    ADD COLUMN IF NOT EXISTS approval_mode TEXT NOT NULL DEFAULT 'manual'
        CONSTRAINT conversations_approval_mode_check
        CHECK (approval_mode IN ('manual', 'auto', 'autonomous'));

UPDATE chat.conversations
   SET approval_mode = 'auto'
 WHERE auto_approve = TRUE
   AND approval_mode = 'manual';
