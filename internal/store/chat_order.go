package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxChatOrder bounds one saved chat order. The rail lists every chat in a
// scope, and nobody hand-orders more than this; the cap keeps one row small.
const MaxChatOrder = 500

// ChatOrderPinned is the scope of the person's pinned chats.
const ChatOrderPinned = "pinned"

// chatOrderProjectPrefix starts the scope of the person's chats in a project.
const chatOrderProjectPrefix = "project:"

// ErrBadChatOrderScope rejects a scope that is neither "pinned" nor
// "project:<id>".
var ErrBadChatOrderScope = errors.New(`scope must be "pinned" or "project:<id>"`)

// ChatOrderProjectScope is the order scope for the user's chats in a project.
func ChatOrderProjectScope(projectID string) string { return chatOrderProjectPrefix + projectID }

// parseChatOrderScope returns the project ID for a project scope ("" for the
// pinned scope), or ErrBadChatOrderScope.
func parseChatOrderScope(scope string) (projectID string, err error) {
	if scope == ChatOrderPinned {
		return "", nil
	}
	if id, ok := strings.CutPrefix(scope, chatOrderProjectPrefix); ok && id != "" && len(id) <= 128 {
		return id, nil
	}
	return "", ErrBadChatOrderScope
}

// ChatOrders returns every order the user has saved, by scope (first to last
// within each). A list may name chats that have since left its scope or been
// deleted: readers ignore those.
func (s *Store) ChatOrders(ctx context.Context, userEmail string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, conversation_ids FROM conversation_order WHERE user_email = $1`,
		userEmail,
	)
	if err != nil {
		return nil, fmt.Errorf("read chat orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var scope string
		var raw []byte
		if err := rows.Scan(&scope, &raw); err != nil {
			return nil, err
		}
		ids := []string{}
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, fmt.Errorf("decode chat order %q: %w", scope, err)
		}
		out[scope] = ids
	}
	return out, rows.Err()
}

// SetChatOrder saves the user's order for one scope and returns what it kept.
// Only conversations the user owns and has not deleted, and that are in the
// scope (pinned, or filed in that project), are kept — in the order given and
// without duplicates — so a saved list never names someone else's chat. An ID
// outside that set is dropped rather than refused, since a chat can be
// unpinned or moved in another tab between a drag and its save. A list that
// keeps nothing removes the scope's row.
func (s *Store) SetChatOrder(ctx context.Context, userEmail, scope string, ids []string) ([]string, error) {
	projectID, err := parseChatOrderScope(scope)
	if err != nil {
		return nil, err
	}
	if len(ids) > MaxChatOrder {
		return nil, fmt.Errorf("chat order: at most %d chats", MaxChatOrder)
	}
	inScope := map[string]bool{}
	if len(ids) > 0 {
		q := `SELECT id FROM conversations
		       WHERE user_email = $1 AND deleted_at IS NULL AND id = ANY($2) AND pinned`
		args := []any{userEmail, ids}
		if projectID != "" {
			q = `SELECT id FROM conversations
			      WHERE user_email = $1 AND deleted_at IS NULL AND id = ANY($2) AND project_id = $3`
			args = append(args, projectID)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("check chat order: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			inScope[id] = true
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	kept := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if inScope[id] && !seen[id] {
			seen[id] = true
			kept = append(kept, id)
		}
	}
	if len(kept) == 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM conversation_order WHERE user_email = $1 AND scope = $2`, userEmail, scope,
		); err != nil {
			return nil, fmt.Errorf("clear chat order: %w", err)
		}
		return kept, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO conversation_order (user_email, scope, conversation_ids, updated_at)
		 VALUES ($1, $2, $3::jsonb, $4)
		 ON CONFLICT (user_email, scope) DO UPDATE
		   SET conversation_ids = EXCLUDED.conversation_ids, updated_at = EXCLUDED.updated_at`,
		userEmail, scope, string(raw), time.Now().Unix(),
	); err != nil {
		return nil, fmt.Errorf("save chat order: %w", err)
	}
	return kept, nil
}
