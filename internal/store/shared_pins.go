package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxSharedPins bounds the Pinned group a pin on somebody else's chat may
// grow: the person's own pinned chats and their pins on other people's,
// together. It matches MaxChatOrder: the group is hand-ordered, and an order
// names at most that many chats. Pinning one's own chat is not capped (it
// never was); once the group is full, only that can still add to it.
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
	// The chats shared with the caller: what may be pinned, and (below) which
	// of their existing pins are still live.
	shared, err := s.ListSharedAndMultiplayerConversations(ctx, userEmail)
	if err != nil {
		return err
	}
	open := make([]string, 0, len(shared))
	ok := false
	for i := range shared {
		open = append(open, shared[i].ID)
		ok = ok || shared[i].ID == convID
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
	// Pins on chats that have stopped being shared with the person are
	// retired here, before the count. Such a row is in no list they could
	// unpin it from, so it must not use up room; and left in place it would
	// come back into force if the chat were shared again, past a cap this
	// pin had since filled. (Until the person pins something else, a chat
	// that is shared again does come back pinned.)
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat.shared_conversation_pins
		  WHERE user_email = $1 AND NOT (conversation_id = ANY($2))`,
		userEmail, open,
	); err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	// The access check above was made before the lock. Looked at again here,
	// with the row held, so that a transfer of the chat either finishes first
	// (and this sees its new owner, or that it is shared with nobody) or
	// waits for this pin and then clears it with the rest.
	var owner string
	var stillShared bool
	err = tx.QueryRowContext(ctx,
		`SELECT user_email, (team_visible OR multiplayer) FROM conversations
		  WHERE id = $1 AND deleted_at IS NULL AND archived_at IS NULL
		    FOR SHARE`, convID,
	).Scan(&owner, &stillShared)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (normalizeEmail(owner) == userEmail || !stillShared)) {
		return ErrConversationNotFound
	}
	if err != nil {
		return fmt.Errorf("pin shared chat: %w", err)
	}
	var already bool
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM chat.shared_conversation_pins
		                 WHERE user_email = $1 AND conversation_id = $2),
		        (SELECT count(*) FROM chat.shared_conversation_pins WHERE user_email = $1)
		      + (SELECT count(*) FROM conversations
		          WHERE user_email = $1 AND pinned AND deleted_at IS NULL AND archived_at IS NULL)`,
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

// overlayViewerPins sets, on every conversation in list that callerEmail does
// not own, the pinned flag to the caller's own pin on it, and then puts what
// is pinned for the caller first (each group in the order it arrived, which
// callers make most recent first). The owner's pin is the owner's
// arrangement: it is not shown to anyone else, and it must not order their
// list either. The caller's own chats keep theirs.
func (s *Store) overlayViewerPins(ctx context.Context, callerEmail string, list []Conversation) error {
	if len(list) == 0 {
		return nil
	}
	callerEmail = normalizeEmail(callerEmail)
	pins, err := s.sharedPinSet(ctx, callerEmail)
	if err != nil {
		return err
	}
	for i := range list {
		if normalizeEmail(list[i].UserEmail) != callerEmail {
			list[i].Pinned = pins[list[i].ID]
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Pinned && !list[j].Pinned })
	return nil
}
