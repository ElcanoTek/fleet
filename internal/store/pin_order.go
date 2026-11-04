package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxPinOrder bounds a saved pinned-chat order. The rail lists every pinned
// chat, and nobody hand-orders more than this; the cap keeps one row small.
const MaxPinOrder = 500

// PinOrder returns the order the user has given their pinned chats (first to
// last), or an empty list when they have never reordered. The list may name
// chats that have since been unpinned or deleted: readers ignore those.
func (s *Store) PinOrder(ctx context.Context, userEmail string) ([]string, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_ids FROM conversation_pin_order WHERE user_email = $1`,
		userEmail,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pin order: %w", err)
	}
	ids := []string{}
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("decode pin order: %w", err)
	}
	return ids, nil
}

// SetPinOrder saves the user's pinned-chat order. Only conversations the user
// owns, has pinned and has not deleted are kept, in the order given and
// without duplicates, so the saved list never names someone else's chat; an
// unknown or foreign ID is dropped rather than refused, since a chat can be
// unpinned in another tab between the drag and the save.
func (s *Store) SetPinOrder(ctx context.Context, userEmail string, ids []string) ([]string, error) {
	if len(ids) > MaxPinOrder {
		return nil, fmt.Errorf("pin order: at most %d chats", MaxPinOrder)
	}
	owned := map[string]bool{}
	if len(ids) > 0 {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id FROM conversations
			  WHERE user_email = $1 AND pinned AND deleted_at IS NULL AND id = ANY($2)`,
			userEmail, ids,
		)
		if err != nil {
			return nil, fmt.Errorf("check pin order: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			owned[id] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	kept := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if owned[id] && !seen[id] {
			seen[id] = true
			kept = append(kept, id)
		}
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_pin_order (user_email, conversation_ids, updated_at)
		 VALUES ($1, $2::jsonb, $3)
		 ON CONFLICT (user_email) DO UPDATE
		   SET conversation_ids = EXCLUDED.conversation_ids, updated_at = EXCLUDED.updated_at`,
		userEmail, string(raw), time.Now().Unix(),
	); err != nil {
		return nil, fmt.Errorf("save pin order: %w", err)
	}
	return kept, nil
}
