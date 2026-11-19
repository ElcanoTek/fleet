package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
// ErrConversationNotFound, exactly what a wrong ID answers. Unpinning removes
// the caller's own row whatever has happened to the chat since (a pin on a
// chat that stopped being shared can always be taken off); with no row to
// remove it is ErrConversationNotFound too, unless the chat is shared with
// them, when unpinning twice is quiet.
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
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM chat.shared_conversation_pins WHERE user_email = $1 AND conversation_id = $2`,
			userEmail, convID,
		)
		if err != nil {
			return fmt.Errorf("unpin shared chat: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		// Nothing was pinned. For a chat shared with the caller that is
		// still a success (unpinning twice is quiet); for any other ID it
		// is the not-found a wrong ID gets, as on every conversation route.
		ok, err := s.sharedWith(ctx, userEmail, convID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrConversationNotFound
		}
		return nil
	}
	ok, err := s.sharedWith(ctx, userEmail, convID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrConversationNotFound
	}
	// One pin at a time per person: the cap is a count followed by an
	// insert, and two pins of different chats at the cap would otherwise
	// both read "one below" and both land. The lock is the transaction's.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('fleet.shared_pins:' || $1, 0))`, userEmail,
	); err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	var already bool
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM chat.shared_conversation_pins
		                 WHERE user_email = $1 AND conversation_id = $2),
		        (SELECT count(*) FROM chat.shared_conversation_pins WHERE user_email = $1)`,
		userEmail, convID,
	).Scan(&already, &count); err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	if already {
		return nil
	}
	if count >= limit {
		return ErrTooManySharedPins
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat.shared_conversation_pins (user_email, conversation_id, pinned_at)
		 VALUES ($1, $2, $3)`,
		userEmail, convID, time.Now().Unix(),
	); err != nil {
		// The account or the chat was deleted since the access check (the
		// table's foreign keys): there is nothing left to pin.
		if pgForeignKeyViolation(err) {
			return ErrConversationNotFound
		}
		return fmt.Errorf("pin shared chat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	return nil
}

// sharedWith reports whether convID is one of the chats other people own
// that userEmail can open: the set ListSharedAndMultiplayerConversations
// returns, which is what the rail lists.
func (s *Store) sharedWith(ctx context.Context, userEmail, convID string) (bool, error) {
	shared, err := s.ListSharedAndMultiplayerConversations(ctx, userEmail)
	if err != nil {
		return false, err
	}
	for i := range shared {
		if shared[i].ID == convID {
			return true, nil
		}
	}
	return false, nil
}

// pgForeignKeyViolation reports whether err wraps a Postgres foreign-key
// violation (SQLSTATE 23503).
func pgForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
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
