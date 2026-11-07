-- 099_conversation_delete_drops_chat_schedules.sql — a conversation's
-- schedules go with it (#2049 M5, ADR-0104).
--
-- Chat schedules moved to the scheduler's schema (sched.chat_schedules, sched
-- migrations 076/077), where conversation_id cannot be a foreign key to this
-- schema's conversations. Without the old ON DELETE CASCADE, every path that
-- hard-deletes conversations (single and bulk delete, the TTL sweep, the
-- unpinned-cap eviction, the soft-delete purge, account erasure, webhook and
-- attachment cleanup, and any added later) would leave the schedules and
-- their run history — the user's prompt text — behind for good.
--
-- One statement-level trigger restores the cascade for all of them at once:
-- after any DELETE on conversations it deletes the schedules of the deleted
-- rows (their run history follows by sched's own FK cascade). It does nothing
-- while the scheduler's table does not exist yet (a chat-only database: a
-- fresh install between the two migration runs, the chat-only CLI verbs).
CREATE OR REPLACE FUNCTION conversations_delete_chat_schedules() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF to_regclass('sched.chat_schedules') IS NOT NULL THEN
        EXECUTE 'DELETE FROM sched.chat_schedules WHERE conversation_id IN (SELECT id FROM deleted_conversations)';
    END IF;
    RETURN NULL;
END
$$;

DROP TRIGGER IF EXISTS conversations_delete_chat_schedules ON conversations;
CREATE TRIGGER conversations_delete_chat_schedules
    AFTER DELETE ON conversations
    REFERENCING OLD TABLE AS deleted_conversations
    FOR EACH STATEMENT
    EXECUTE FUNCTION conversations_delete_chat_schedules();
