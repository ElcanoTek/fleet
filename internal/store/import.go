package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the legacy-import write primitives used by `fleet import`
// (docs/LEGACY-IMPORT.md): inserts that — unlike CreateUser / CreateConversation
// / AppendHistory — preserve the SOURCE system's identity (ids, bcrypt hashes,
// timestamps) instead of minting fresh ones. Every primitive is
// insert-if-absent on the source identity, so re-running an import never
// duplicates or overwrites data that already made it over.

// ImportedUser is one legacy user row: the bcrypt hash and timestamps travel
// verbatim so the account keeps its password. Role is not carried — imports
// land as the column-default 'member' and are promoted afterwards via
// `fleet chat user role`.
type ImportedUser struct {
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// ImportedMessage is one legacy message: content is the raw JSON blob exactly
// as it sat in the source messages.content column.
type ImportedMessage struct {
	Role      string          `json:"role"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"`
	CreatedAt int64           `json:"created_at"`
}

// ImportedConversation is one legacy conversation plus its full message
// history, ids and timestamps preserved.
type ImportedConversation struct {
	ID                        string            `json:"id"`
	UserEmail                 string            `json:"user_email"`
	Title                     string            `json:"title"`
	Persona                   string            `json:"persona"`
	Model                     string            `json:"model"`
	Pinned                    bool              `json:"pinned"`
	Lockdown                  bool              `json:"lockdown"`
	OptionalMCPServersEnabled []string          `json:"optional_mcp_servers_enabled,omitempty"`
	CreatedAt                 int64             `json:"created_at"`
	UpdatedAt                 int64             `json:"updated_at"`
	Messages                  []ImportedMessage `json:"messages"`
}

// ImportedMemory is one legacy memory row. conversation_id (only meaningful
// for source='proposed') is nulled by the importer when the conversation
// didn't come along.
type ImportedMemory struct {
	ID             string `json:"id"`
	UserEmail      string `json:"user_email"`
	Content        string `json:"content"`
	Source         string `json:"source"`
	ConversationID string `json:"conversation_id,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// ImportUser inserts a legacy user with its bcrypt hash + timestamps
// preserved. Returns false (skipped) when the email already exists — an
// existing fleet account always wins so an import can never rotate someone's
// password out from under them.
func (s *Store) ImportUser(ctx context.Context, u ImportedUser) (bool, error) {
	email := normalizeEmail(u.Email)
	if email == "" {
		return false, fmt.Errorf("import user: empty email")
	}
	if strings.TrimSpace(u.PasswordHash) == "" {
		return false, fmt.Errorf("import user %s: empty password_hash", email)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (email) DO NOTHING`,
		email, u.PasswordHash, u.CreatedAt, u.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("import user %s: %w", email, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ImportConversation inserts a legacy conversation and its messages in one
// transaction, preserving the conversation UUID and all timestamps. Returns
// false (skipped) when the conversation id already exists — the whole
// conversation, messages included, is left untouched, which is what makes
// re-running an import idempotent. FTS side-table rows are NOT written here;
// callers run BackfillSearchContent once after the batch (it picks up every
// imported message).
func (s *Store) ImportConversation(ctx context.Context, c ImportedConversation) (bool, error) {
	if strings.TrimSpace(c.ID) == "" {
		return false, fmt.Errorf("import conversation: empty id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	mcp := c.OptionalMCPServersEnabled
	if mcp == nil {
		mcp = []string{}
	}
	mcpJSON, err := json.Marshal(mcp)
	if err != nil {
		return false, fmt.Errorf("import conversation %s: marshal mcp opt-ins: %w", c.ID, err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO conversations
		   (id, user_email, title, persona, model, pinned, lockdown,
		    optional_mcp_servers_enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (id) DO NOTHING`,
		c.ID, normalizeEmail(c.UserEmail), c.Title, c.Persona, c.Model,
		c.Pinned, c.Lockdown, string(mcpJSON), c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("import conversation %s: %w", c.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already migrated — skip whole conversation
	}

	// Messages, batched to keep parameter counts bounded (5 params/row;
	// Postgres caps at 65535). Per-row created_at preserves turn timing.
	const batch = 5000
	for start := 0; start < len(c.Messages); start += batch {
		end := start + batch
		if end > len(c.Messages) {
			end = len(c.Messages)
		}
		if err := importMessagesTx(ctx, tx, c.ID, c.Messages[start:end]); err != nil {
			return false, fmt.Errorf("import conversation %s: %w", c.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import conversation %s: commit: %w", c.ID, err)
	}
	return true, nil
}

func importMessagesTx(ctx context.Context, tx *sql.Tx, convID string, msgs []ImportedMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES `)
	args := make([]any, 0, len(msgs)*5)
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*5 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d)", base, base+1, base+2, base+3, base+4)
		args = append(args, convID, m.Role, m.Type, string(m.Content), m.CreatedAt)
	}
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}

// ImportMemory inserts a legacy memory, insert-if-absent by id. learned_at is
// backfilled from created_at and kind defaults to 'fact', exactly matching
// what migration 026_typed_memories.sql did to pre-existing rows — so a
// migrated memory is indistinguishable from one that lived here all along.
// convExists tells the importer whether the referenced conversation made it
// over; when false the reference is dropped (it only matters for pending
// proposals).
func (s *Store) ImportMemory(ctx context.Context, m ImportedMemory, convExists bool) (bool, error) {
	if strings.TrimSpace(m.ID) == "" {
		return false, fmt.Errorf("import memory: empty id")
	}
	var convID any
	if m.ConversationID != "" && convExists {
		convID = m.ConversationID
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO memories
		   (id, user_email, content, source, conversation_id, created_at, updated_at, learned_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $6)
		 ON CONFLICT (id) DO NOTHING`,
		m.ID, normalizeEmail(m.UserEmail), m.Content, m.Source, convID, m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("import memory %s: %w", m.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HasConversation reports whether a conversation id exists (used by the
// importer to resolve memory → conversation references).
func (s *Store) HasConversation(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id = $1`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
