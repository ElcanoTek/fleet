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

// ChatOrderPinnedProjectPrefix starts a pinned PROJECT's entry in the pinned
// order. The sidebar's Pinned section lists pinned chats and pinned projects
// together, in one order the person sets, so the one saved list names both: a
// chat by its ID, a project as "project:<project id>".
const ChatOrderPinnedProjectPrefix = "project:"

// ChatOrderPinnedProject is a pinned project's entry in the pinned order.
func ChatOrderPinnedProject(projectID string) string {
	return ChatOrderPinnedProjectPrefix + projectID
}

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
// Only live conversations that are in the scope are kept — in the order given
// and without duplicates. The pinned scope is the user's own pinned chats plus
// the chats other people own that the user has pinned for themselves
// (SetSharedPin, which accepts only a chat shared with them), and the pinned
// projects the user can see, each named "project:<id>"
// (ChatOrderPinnedProject); a project scope is the user's own chats filed in
// that project. So a saved list never names a chat the user did not put in
// the scope themselves, nor a project they cannot see. An ID
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
		// Pinned: the person's own pinned chats, and the chats other people
		// own that they have pinned for themselves (migration 0204) — one
		// Pinned group in the rail, so one order.
		q := `SELECT id FROM conversations
		       WHERE user_email = $1 AND deleted_at IS NULL AND id = ANY($2) AND pinned
		      UNION
		      SELECT p.conversation_id FROM chat.shared_conversation_pins p
		        JOIN conversations c ON c.id = p.conversation_id AND c.deleted_at IS NULL
		       WHERE p.user_email = $3 AND p.conversation_id = ANY($2)`
		args := []any{userEmail, ids, normalizeEmail(userEmail)}
		if projectID != "" {
			q = `SELECT id FROM conversations
			      WHERE user_email = $1 AND deleted_at IS NULL AND id = ANY($2) AND project_id = $3`
			args = []any{userEmail, ids, projectID}
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
		if projectID == "" {
			if err := s.pinnedProjectEntries(ctx, userEmail, ids, inScope); err != nil {
				return nil, err
			}
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

// pinnedProjectEntries marks, in inScope, the "project:<id>" entries of ids
// that name a pinned project userEmail can see: one they own, or one shared
// with their team (the rule ListProjectsForUser applies). A project's pin is
// its owner's, so a member orders a project the owner pinned; one that is
// unpinned, deleted or no longer shared with them is not in scope and is
// dropped by the caller.
func (s *Store) pinnedProjectEntries(ctx context.Context, userEmail string, ids []string, inScope map[string]bool) error {
	projectIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if pid, ok := strings.CutPrefix(id, ChatOrderPinnedProjectPrefix); ok && pid != "" {
			projectIDs = append(projectIDs, pid)
		}
	}
	if len(projectIDs) == 0 {
		return nil
	}
	team := ""
	if u, err := s.GetUser(ctx, userEmail); err == nil && u != nil {
		team = strings.TrimSpace(u.TeamID)
	} else if err != nil && !errors.Is(err, ErrUserNotFound) {
		return fmt.Errorf("check chat order: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM projects
		  WHERE id = ANY($1) AND pinned
		    AND (owner_email = $2 OR (team_id != '' AND team_id = $3))`,
		projectIDs, normalizeEmail(userEmail), team,
	)
	if err != nil {
		return fmt.Errorf("check chat order: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		inScope[ChatOrderPinnedProject(id)] = true
	}
	return rows.Err()
}
