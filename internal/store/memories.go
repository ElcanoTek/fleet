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

// Memory is a user-scoped typed memory record injected into future turns
// (#515 MVP). Beyond the flat content string it carries:
//
//   - Kind — what SORT of fact this is (fact/preference/identity/constraint/
//     context), so retrieval and review are legible.
//   - Provenance — Source (manual|chat|proposed), Origin (manual|tool|auto:
//     who wrote it), ConversationID (where it came from; retained after
//     accept), and LearnedAt (TRANSACTION time: when fleet recorded it).
//   - Validity — ValidFrom/ValidTo (VALID time: when the fact is true in the
//     world, user-editable, nil = open-ended). Deliberately distinct from
//     retirement, which is a transaction-time audit event.
//   - Retirement — RetiredAt set = excluded from prompt injection but kept for
//     audit/restore; RetiredBy links the superseding memory when the stage-2
//     contradiction flow retired it (empty for manual retirement).
//   - Pinned — always injected first and protected from supersede-retirement.
type Memory struct {
	ID             string `json:"id"`
	UserEmail      string `json:"user_email"`
	Content        string `json:"content"`
	Source         string `json:"source"`
	Kind           string `json:"kind"`
	Origin         string `json:"origin,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Pinned         bool   `json:"pinned"`
	ValidFrom      *int64 `json:"valid_from,omitempty"`
	ValidTo        *int64 `json:"valid_to,omitempty"`
	LearnedAt      int64  `json:"learned_at"`
	RetiredAt      *int64 `json:"retired_at,omitempty"`
	RetiredBy      string `json:"retired_by,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// Retired reports whether the memory is soft-retired (excluded from injection).
func (m *Memory) Retired() bool { return m.RetiredAt != nil }

// memoryColumns is the shared SELECT list every memory scan uses — one source
// of truth so a new column can't be read in one query and missed in another.
const memoryColumns = `id, user_email, content, source, kind, origin,
	COALESCE(conversation_id, ''), pinned, valid_from, valid_to,
	COALESCE(learned_at, created_at), retired_at, COALESCE(retired_by, ''),
	created_at, updated_at`

func scanMemory(scanner interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	if err := scanner.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.Kind, &m.Origin,
		&m.ConversationID, &m.Pinned, &m.ValidFrom, &m.ValidTo,
		&m.LearnedAt, &m.RetiredAt, &m.RetiredBy,
		&m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	return &m, nil
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

// memoryKinds is the closed set of memory types. Unknown values normalize to
// "fact" so an over-creative model or an old client can never poison the
// column — typing stays legible without a hard failure path.
var memoryKinds = map[string]bool{
	"fact":       true,
	"preference": true,
	"identity":   true,
	"constraint": true,
	"context":    true,
}

// NormalizeMemoryKind maps any input onto the closed kind set ("" and unknown
// values become "fact"). Exported so the HTTP layer and the proposal gates
// share one normalization.
func NormalizeMemoryKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !memoryKinds[kind] {
		return "fact"
	}
	return kind
}

func normalizeMemoryOrigin(origin string) string {
	switch strings.ToLower(strings.TrimSpace(origin)) {
	case "tool":
		return "tool"
	case "auto":
		return "auto"
	default:
		return "manual"
	}
}

func (s *Store) CreateMemory(ctx context.Context, userEmail, content, source, kind string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	source = normalizeMemorySource(source)
	kind = NormalizeMemoryKind(kind)
	id := uuid.NewString()
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO memories (id, user_email, content, source, kind, origin, learned_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'manual', $6, $6, $6)
		 RETURNING `+memoryColumns,
		id, normalizeEmail(userEmail), content, source, kind, now,
	)
	return scanMemory(row)
}

// ListMemories returns every memory for the user — active first (pinned, then
// freshest), retired rows trailing so the manager UI can render them in a
// separate section. The injection path (httpapi memoryContents) filters
// retired/proposed rows and caps the count.
func (s *Store) ListMemories(ctx context.Context, userEmail string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+`
		 FROM memories WHERE user_email = $1
		 ORDER BY (retired_at IS NOT NULL) ASC, pinned DESC, updated_at DESC, id DESC`,
		normalizeEmail(userEmail),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// MemoryPatch is a partial update: nil fields are untouched. ValidFrom /
// ValidTo use 0 to CLEAR the bound (0 is not a meaningful epoch for a fact's
// validity). Retired true sets retired_at=now (manual retirement, no
// retired_by); false restores (clears retired_at AND retired_by).
type MemoryPatch struct {
	Content   *string
	Kind      *string
	Pinned    *bool
	Retired   *bool
	ValidFrom *int64
	ValidTo   *int64
}

// UpdateMemory applies a partial update to a user's memory. An empty patch is
// an error (the caller sent nothing to change).
func (s *Store) UpdateMemory(ctx context.Context, userEmail, id string, patch MemoryPatch) (*Memory, error) {
	if patch.Content == nil && patch.Kind == nil && patch.Pinned == nil &&
		patch.Retired == nil && patch.ValidFrom == nil && patch.ValidTo == nil {
		return nil, errors.New("empty memory patch")
	}
	var content *string
	if patch.Content != nil {
		c := normalizeMemoryContent(*patch.Content)
		if c == "" {
			return nil, errors.New("memory content required")
		}
		content = &c
	}
	var kind *string
	if patch.Kind != nil {
		k := NormalizeMemoryKind(*patch.Kind)
		kind = &k
	}
	now := time.Now().Unix()

	// Sentinel-guarded single UPDATE (the ListFiltered pattern): each clause
	// applies only when its parameter is non-NULL, so a partial patch is one
	// statement with no SQL assembly. valid_from/valid_to use 0-as-clear.
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET
			content    = COALESCE($1::text, content),
			kind       = COALESCE($2::text, kind),
			pinned     = COALESCE($3::boolean, pinned),
			retired_at = CASE
				WHEN $4::boolean IS NULL THEN retired_at
				WHEN $4::boolean THEN COALESCE(retired_at, $5)
				ELSE NULL END,
			retired_by = CASE
				WHEN $4::boolean IS NULL THEN retired_by
				WHEN $4::boolean THEN retired_by
				ELSE NULL END,
			valid_from = CASE
				WHEN $6::bigint IS NULL THEN valid_from
				WHEN $6::bigint = 0 THEN NULL
				ELSE $6::bigint END,
			valid_to = CASE
				WHEN $7::bigint IS NULL THEN valid_to
				WHEN $7::bigint = 0 THEN NULL
				ELSE $7::bigint END,
			updated_at = $5
		 WHERE id = $8 AND user_email = $9
		 RETURNING `+memoryColumns,
		content, kind, patch.Pinned, patch.Retired, now, patch.ValidFrom, patch.ValidTo,
		id, normalizeEmail(userEmail),
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory not found")
		}
		return nil, err
	}
	return m, nil
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
// transient client-side proposal state. origin records WHO proposed it:
// "tool" (the agent's propose_memory call) or "auto" (the post-turn
// extractor, #234) — provenance the explainability contract requires.
func (s *Store) CreateMemoryProposal(ctx context.Context, userEmail, conversationID, content, kind, origin string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	kind = NormalizeMemoryKind(kind)
	origin = normalizeMemoryOrigin(origin)
	id := uuid.NewString()
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO memories (id, user_email, conversation_id, content, source, kind, origin, learned_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'proposed', $5, $6, $7, $7, $7)
		 RETURNING `+memoryColumns,
		id, normalizeEmail(userEmail), conversationID, content, kind, origin, now,
	)
	return scanMemory(row)
}

// AcceptMemoryProposal changes a proposed memory's source to "chat" so it
// becomes a saved (global) memory. conversation_id is RETAINED as provenance
// (#515: "who/what wrote it" must survive acceptance) — the pending-proposal
// queries filter on source='proposed', so a retained id no longer marks the
// row as pending.
func (s *Store) AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*Memory, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET source = 'chat', updated_at = $1
		 WHERE id = $2 AND user_email = $3 AND source = 'proposed'
		 RETURNING `+memoryColumns,
		now, id, normalizeEmail(userEmail),
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory proposal not found")
		}
		return nil, err
	}
	return m, nil
}

// ListPendingMemoryProposalsForConversation returns proposals (source='proposed')
// for a specific conversation, oldest first. The HTTP layer feeds this into
// the conversation-load response so the UI can re-render the Save/Don't-Save
// card after a focus event or page refresh re-fetches messages.
func (s *Store) ListPendingMemoryProposalsForConversation(ctx context.Context, userEmail, conversationID string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+`
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
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
