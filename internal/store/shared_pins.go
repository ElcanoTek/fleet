package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaxSharedPins bounds how many chats owned by other people one person may
// pin. It matches MaxChatOrder: the pinned group is hand-ordered, and an
// order names at most that many chats.
const MaxSharedPins = MaxChatOrder

// ErrTooManySharedPins refuses a pin past MaxSharedPins.
var ErrTooManySharedPins = errors.New("too many pinned chats")

// SetSharedPin pins or unpins, for userEmail alone, a chat somebody else owns
// (migration 0204). conversations.pinned is the owner's pin and is not
// touched: the owner's list, the retention sweep and every other viewer are
// unaffected.
//
// A pin is accepted only for a chat ListSharedAndMultiplayerConversations
// returns for the caller — the same set the rail lists — so the table never
// names a chat its owner did not share with them; anything else is
// ErrConversationNotFound, exactly what a wrong ID answers. Unpinning needs
// no such check: removing one's own row reveals nothing.
func (s *Store) SetSharedPin(ctx context.Context, userEmail, convID string, pinned bool) error {
	return s.setSharedPin(ctx, userEmail, convID, pinned, MaxSharedPins)
}

// setSharedPin is SetSharedPin with the cap as a parameter, so a test can
// reach it without making five hundred chats.
func (s *Store) setSharedPin(ctx context.Context, userEmail, convID string, pinned bool, limit int) error {
	userEmail = normalizeEmail(userEmail)
	if userEmail == "" || convID == "" {
		return ErrConversationNotFound
	}
	if !pinned {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM chat.shared_conversation_pins WHERE user_email = $1 AND conversation_id = $2`,
			userEmail, convID,
		); err != nil {
			return fmt.Errorf("unpin shared chat: %w", err)
		}
		return nil
	}
	shared, err := s.ListSharedAndMultiplayerConversations(ctx, userEmail)
	if err != nil {
		return err
	}
	found := false
	for i := range shared {
		if shared[i].ID == convID {
			found = true
			break
		}
	}
	if !found {
		return ErrConversationNotFound
	}
	// The count and the insert are one statement, so two pins racing at the
	// cap cannot both land. Re-pinning an already pinned chat is a no-op.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO chat.shared_conversation_pins (user_email, conversation_id, pinned_at)
		 SELECT $1, $2, $3
		  WHERE EXISTS (SELECT 1 FROM chat.shared_conversation_pins
		                 WHERE user_email = $1 AND conversation_id = $2)
		     OR (SELECT count(*) FROM chat.shared_conversation_pins WHERE user_email = $1) < $4
		 ON CONFLICT (user_email, conversation_id) DO NOTHING`,
		userEmail, convID, time.Now().Unix(), limit,
	)
	if err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM chat.shared_conversation_pins
			                 WHERE user_email = $1 AND conversation_id = $2)`,
			userEmail, convID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("pin shared chat: %w", err)
		}
		if !exists {
			return ErrTooManySharedPins
		}
	}
	return nil
}

// SharedPinned reports whether userEmail has pinned convID, a chat somebody
// else owns. It says nothing about access: callers use it only to fill in the
// pinned flag of a chat they have already decided the person may read.
func (s *Store) SharedPinned(ctx context.Context, userEmail, convID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var pinned bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM chat.shared_conversation_pins
		                 WHERE user_email = $1 AND conversation_id = $2)`,
		normalizeEmail(userEmail), convID,
	).Scan(&pinned)
	if err != nil {
		return false, fmt.Errorf("read shared pin: %w", err)
	}
	return pinned, nil
}

// sharedPinSet returns the IDs of the chats userEmail has pinned that other
// people own.
func (s *Store) sharedPinSet(ctx context.Context, userEmail string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT conversation_id FROM chat.shared_conversation_pins WHERE user_email = $1`,
		normalizeEmail(userEmail),
	)
	if err != nil {
		return nil, fmt.Errorf("read shared pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
