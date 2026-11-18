package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Handing a chat to another person (docs/CHAT-TRANSFER.md).

var (
	// ErrTransferTarget is returned when the person named cannot take a chat:
	// no account here, a disabled one, a read-only viewer, or the owner
	// themselves.
	ErrTransferTarget = errors.New("that person can't take this chat: they need an active Fleet account that can chat")
	// ErrTransferNotAllowed is returned for a chat that cannot change hands:
	// an archived one or a Lockdown one.
	ErrTransferNotAllowed = errors.New("an archived or Lockdown chat can't be transferred")
)

// ConversationTransfer reports what TransferConversation did.
type ConversationTransfer struct {
	Conversation *Conversation
	FromEmail    string
	ToEmail      string
	// PatrolsMoved is how many of the previous owner's patrols and timers in
	// the chat now belong to the new owner; PatrolsPaused is how many of them
	// were running and are now paused.
	PatrolsMoved  int
	PatrolsPaused int
}

// TransferConversation makes toEmail the owner of ownerEmail's conversation,
// in one transaction:
//
//   - The chat becomes private to its new owner. Team sharing, Multiplayer
//     mode with its participant lists, and the public link are the previous
//     owner's decisions about their audience, so none of them carries over;
//     the new owner shares it again as they see fit.
//   - What was the previous owner's own arrangement is reset: pin, labels,
//     project filing, connector account choices, and the approval mode (back
//     to Manual — Auto-approve and Autonomous are each owner's own call).
//   - The previous owner's patrols and timers in the chat move to the new
//     owner and are PAUSED. A patrol runs with its owner's connectors, so it
//     starts again only when the new owner resumes it. A fire in flight is
//     revoked and every queued input in the chat is cancelled.
//
// The transcript, the workspace files and the chat's persona and model are
// untouched. Other people's patrols in the chat are not moved; the caller
// reconciles them (and any running turn) with the chat's new, private
// audience. The target must be an enabled, non-viewer account.
func (s *Store) TransferConversation(ctx context.Context, ownerEmail, convID, toEmail string) (*ConversationTransfer, error) {
	from, to := normalizeEmail(ownerEmail), normalizeEmail(toEmail)
	if to == "" || to == from {
		return nil, ErrTransferTarget
	}
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		archivedAt sql.NullInt64
		lockdown   bool
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT archived_at, lockdown FROM conversations
		  WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL
		    FOR UPDATE`, convID, from,
	).Scan(&archivedAt, &lockdown); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, err
	}
	if archivedAt.Valid || lockdown {
		return nil, ErrTransferNotAllowed
	}
	// FOR SHARE: a concurrent disable or delete of the target waits, or has
	// already happened and is refused here.
	var one int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM core.users WHERE email = $1 AND enabled = TRUE AND role <> 'viewer' FOR SHARE`, to,
	).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTransferTarget
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations SET
			user_email = $1,
			pinned = FALSE,
			labels = '{}',
			project_id = NULL,
			share_token = NULL, shared_at = NULL, share_expires_at = NULL,
			`+unshareConversationSet+`,
			multiplayer_participants = '[]', multiplayer_excluded = '[]',
			mcp_accounts = '{}',
			auto_approve = FALSE, approval_mode = 'manual',
			updated_at = $2
		WHERE id = $3`, to, now, convID); err != nil {
		return nil, err
	}

	out := &ConversationTransfer{FromEmail: from, ToEmail: to}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE status = $1)
		   FROM `+chatSchedulesTable+`
		  WHERE conversation_id = $3 AND user_email = $4 AND status IN ($1, $2)`,
		ChatScheduleStatusActive, ChatScheduleStatusPaused, convID, from,
	).Scan(&out.PatrolsMoved, &out.PatrolsPaused); err != nil {
		return nil, err
	}
	// Pausing also revokes a fire in flight, as a pause from the Patrol bar
	// does (UpdateChatSchedule): it settles to nothing.
	if _, err := tx.ExecContext(ctx, `
		UPDATE `+chatSchedulesTable+` SET
			user_email = $1,
			status = $3,
			lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL,
			fire_trigger = NULL, fire_run_number = NULL, fire_failures = 0,
			updated_at = $4
		WHERE conversation_id = $5 AND user_email = $6 AND status IN ($2, $3)`,
		to, ChatScheduleStatusActive, ChatScheduleStatusPaused, now, convID, from,
	); err != nil {
		return nil, err
	}
	// Nothing queued by the previous audience runs in the new owner's chat.
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE conversation_id = $2 AND state = 'queued'`, now, convID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	conv, err := s.Get(ctx, to, convID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, ErrConversationNotFound
	}
	out.Conversation = conv
	return out, nil
}
