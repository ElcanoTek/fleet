package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxMemoryContentRunes = 4000

// Memory is a user-scoped fact or preference injected into future turns.
type Memory struct {
	ID        string `json:"id"`
	UserEmail string `json:"user_email"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func normalizeMemoryContent(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	runes := []rune(content)
	if len(runes) > maxMemoryContentRunes {
		content = string(runes[:maxMemoryContentRunes])
	}
	return strings.TrimSpace(content)
}

// memorySourceChat is the source value for saved (chat-created or
// user-accepted) memories, as opposed to "manual" and "proposed".
const memorySourceChat = "chat"

func normalizeMemorySource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source != memorySourceChat {
		return "manual"
	}
	return source
}

func (s *Store) CreateMemory(ctx context.Context, userEmail, content, source string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	source = normalizeMemorySource(source)
	id := uuid.NewString()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, user_email, content, source, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, normalizeEmail(userEmail), content, source, now, now,
	)
	if err != nil {
		return nil, err
	}
	return &Memory{ID: id, UserEmail: normalizeEmail(userEmail), Content: content, Source: source, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) ListMemories(ctx context.Context, userEmail string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_email, content, source, created_at, updated_at
		 FROM memories WHERE user_email = $1 ORDER BY updated_at DESC, id DESC`,
		normalizeEmail(userEmail),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpdateMemory(ctx context.Context, userEmail, id, content string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET content = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4
		 RETURNING id, user_email, content, source, created_at, updated_at`,
		content, now, id, normalizeEmail(userEmail),
	)
	var m Memory
	if err := row.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory not found")
		}
		return nil, err
	}
	return &m, nil
}

func (s *Store) DeleteMemory(ctx context.Context, userEmail, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE id = $1 AND user_email = $2`,
		id, normalizeEmail(userEmail),
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("memory not found")
	}
	return nil
}

// CreateMemoryProposal creates a memory with source="proposed" scoped to
// the conversation it was proposed in. The conversation_id lets the UI
// re-hydrate the Save/Don't-Save card on conversation load — without it,
// any focus/visibility event triggers a loadConversation that wipes the
// transient client-side proposal state.
func (s *Store) CreateMemoryProposal(ctx context.Context, userEmail, conversationID, content string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	id := uuid.NewString()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, user_email, conversation_id, content, source, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'proposed', $5, $6)`,
		id, normalizeEmail(userEmail), conversationID, content, now, now,
	)
	if err != nil {
		return nil, err
	}
	return &Memory{ID: id, UserEmail: normalizeEmail(userEmail), Content: content, Source: "proposed", CreatedAt: now, UpdatedAt: now}, nil
}

// AcceptMemoryProposal changes a proposed memory's source to "chat" so it
// becomes a saved (global) memory. Clears conversation_id since accepted
// memories are user-scoped, not conversation-scoped.
func (s *Store) AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*Memory, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET source = 'chat', conversation_id = NULL, updated_at = $1
		 WHERE id = $2 AND user_email = $3 AND source = 'proposed'
		 RETURNING id, user_email, content, source, created_at, updated_at`,
		now, id, normalizeEmail(userEmail),
	)
	var m Memory
	if err := row.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory proposal not found")
		}
		return nil, err
	}
	return &m, nil
}

// ListPendingMemoryProposalsForConversation returns proposals (source='proposed')
// for a specific conversation, oldest first. The HTTP layer feeds this into
// the conversation-load response so the UI can re-render the Save/Don't-Save
// card after a focus event or page refresh re-fetches messages.
func (s *Store) ListPendingMemoryProposalsForConversation(ctx context.Context, userEmail, conversationID string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_email, content, source, created_at, updated_at
		 FROM memories
		 WHERE user_email = $1 AND conversation_id = $2 AND source = 'proposed'
		 ORDER BY created_at ASC, id ASC`,
		normalizeEmail(userEmail), conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
