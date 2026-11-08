package db

import (
	"context"
	"fmt"
)

// The conversation-delete cascade for chat schedules (ADR-0104). The rows live
// in this schema and their conversations in chat's, so there is no foreign key:
// a statement-level AFTER DELETE trigger on chat.conversations deletes the
// deleted conversations' schedules (their run history follows by this
// schema's FK). Sched migration 077 installs it when chat.conversations
// already exists — the normal order, the chat store migrating first. On a
// database the scheduler initialised first (a sched-only CLI verb before any
// `fleet serve`), 077 ran with no chat schema to attach to, so
// EnsureChatScheduleCascade installs it at serve startup, once both schemas
// exist. It is deliberately not a chat migration: a rollback (sched downs
// 077, 076) must leave the chat schema at a version the previous release
// knows, and 077's down drops what either path installed.

// chatScheduleCascadeDDL is the trigger and its function, as 077 creates them.
const chatScheduleCascadeDDL = `
CREATE OR REPLACE FUNCTION chat.conversations_delete_chat_schedules() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF to_regclass('sched.chat_schedules') IS NOT NULL THEN
        EXECUTE 'DELETE FROM sched.chat_schedules WHERE conversation_id IN (SELECT id FROM deleted_conversations)';
    END IF;
    RETURN NULL;
END
$fn$;

DROP TRIGGER IF EXISTS conversations_delete_chat_schedules ON chat.conversations;
CREATE TRIGGER conversations_delete_chat_schedules
    AFTER DELETE ON chat.conversations
    REFERENCING OLD TABLE AS deleted_conversations
    FOR EACH STATEMENT
    EXECUTE FUNCTION chat.conversations_delete_chat_schedules();
`

// EnsureChatScheduleCascade installs the conversation-delete cascade when both
// chat.conversations and this schema's chat_schedules exist and the trigger
// is missing, and in the same transaction deletes the schedules (and so their
// history) whose conversation was deleted while it was missing. A no-op when
// the trigger is already there or either table is absent. Idempotent.
// Returns whether it installed the trigger and how many orphans it removed.
func (db *Database) EnsureChatScheduleCascade(ctx context.Context) (installed bool, orphans int, err error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Serialise concurrent boots on one database.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('fleet:chat-schedule-cascade'))`); err != nil {
		return false, 0, err
	}
	var ready, present bool
	if err := tx.QueryRowContext(ctx, `
		SELECT to_regclass('chat.conversations') IS NOT NULL AND to_regclass('chat_schedules') IS NOT NULL`,
	).Scan(&ready); err != nil {
		return false, 0, err
	}
	if !ready {
		return false, 0, nil
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_trigger
		                WHERE tgname = 'conversations_delete_chat_schedules'
		                  AND tgrelid = 'chat.conversations'::regclass)`,
	).Scan(&present); err != nil {
		return false, 0, err
	}
	if present {
		return false, 0, nil
	}
	if _, err := tx.ExecContext(ctx, chatScheduleCascadeDDL); err != nil {
		return false, 0, fmt.Errorf("install the conversation-delete cascade: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM chat_schedules s
		 WHERE NOT EXISTS (SELECT 1 FROM chat.conversations c WHERE c.id = s.conversation_id)`)
	if err != nil {
		return false, 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, int(n), nil
}
