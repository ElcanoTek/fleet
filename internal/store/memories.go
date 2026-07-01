package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	// Supersedes (proposals only) names the saved memory this proposal claims
	// to contradict/outdate; SupersedesHash snapshots that target's content at
	// claim time so an edited target is never retired on a stale justification.
	Supersedes     string `json:"supersedes,omitempty"`
	SupersedesHash string `json:"-"`
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
	COALESCE(supersedes, ''), COALESCE(supersedes_hash, ''),
	created_at, updated_at`

func scanMemory(scanner interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	if err := scanner.Scan(&m.ID, &m.UserEmail, &m.Content, &m.Source, &m.Kind, &m.Origin,
		&m.ConversationID, &m.Pinned, &m.ValidFrom, &m.ValidTo,
		&m.LearnedAt, &m.RetiredAt, &m.RetiredBy,
		&m.Supersedes, &m.SupersedesHash,
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

// MemoryProposalParams shape one memory proposal. Supersedes (optional, set
// by the auto-extractor's contradiction candidates, #515 stage 2) names the
// SAVED memory this proposal claims to replace; SupersedesHash must be
// MemoryContentHash of that target's content at claim time.
type MemoryProposalParams struct {
	Content        string
	Kind           string
	Origin         string
	Supersedes     string
	SupersedesHash string
}

// MemoryContentHash is the snapshot hash stored alongside a supersede claim —
// at accept time a differing hash means the target was edited after the claim
// and must NOT be retired on the stale justification.
func MemoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// CreateMemoryProposal creates a memory with source="proposed" scoped to
// the conversation it was proposed in. The conversation_id lets the UI
// re-hydrate the Save/Don't-Save card on conversation load — without it,
// any focus/visibility event triggers a loadConversation that wipes the
// transient client-side proposal state. p.Origin records WHO proposed it:
// "tool" (the agent's propose_memory call) or "auto" (the post-turn
// extractor, #234) — provenance the explainability contract requires.
func (s *Store) CreateMemoryProposal(ctx context.Context, userEmail, conversationID string, p MemoryProposalParams) (*Memory, error) {
	content := normalizeMemoryContent(p.Content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	kind := NormalizeMemoryKind(p.Kind)
	origin := normalizeMemoryOrigin(p.Origin)
	var supersedes, supersedesHash *string
	if strings.TrimSpace(p.Supersedes) != "" {
		sup, hash := p.Supersedes, p.SupersedesHash
		supersedes, supersedesHash = &sup, &hash
	}
	id := uuid.NewString()
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO memories (id, user_email, conversation_id, content, source, kind, origin, supersedes, supersedes_hash, learned_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'proposed', $5, $6, $7, $8, $9, $9, $9)
		 RETURNING `+memoryColumns,
		id, normalizeEmail(userEmail), conversationID, content, kind, origin, supersedes, supersedesHash, now,
	)
	return scanMemory(row)
}

// Supersede outcomes for AcceptMemoryProposal. Every guard failure is a
// RESULT, never an error — the acceptance itself still succeeds; the caller
// surfaces what happened to the older fact.
const (
	SupersedeNone           = ""                // proposal made no supersede claim
	SupersedeRetired        = "retired"         // target retired, retired_by = new memory
	SupersedeTargetPinned   = "target_pinned"   // target pinned → kept (user protected it)
	SupersedeTargetChanged  = "target_changed"  // target edited since the claim → kept
	SupersedeTargetMissing  = "target_missing"  // target deleted since the claim
	SupersedeTargetRetired  = "target_retired"  // target already retired by something else
	SupersedeTargetProposed = "target_proposed" // target is itself still a proposal → kept
)

// AcceptMemoryProposal changes a proposed memory's source to "chat" so it
// becomes a saved (global) memory. conversation_id is RETAINED as provenance
// (#515: "who/what wrote it" must survive acceptance) — the pending-proposal
// queries filter on source='proposed', so a retained id no longer marks the
// row as pending.
//
// When the proposal carries a supersede claim (#515 stage 2), the accept and
// the retirement happen in ONE transaction, with the retirement guarded: the
// target must still exist, still be active, not pinned, not itself a
// proposal, and its content must still hash to the claim-time snapshot. Any
// guard failure keeps the target and reports the outcome — a human approved
// "save the new fact", so that always proceeds; retiring the old one on a
// justification that no longer holds does not.
func (s *Store) AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*Memory, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, SupersedeNone, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	row := tx.QueryRowContext(ctx,
		`UPDATE memories SET source = 'chat', updated_at = $1
		 WHERE id = $2 AND user_email = $3 AND source = 'proposed'
		 RETURNING `+memoryColumns,
		now, id, normalizeEmail(userEmail),
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, SupersedeNone, errors.New("memory proposal not found")
		}
		return nil, SupersedeNone, err
	}

	outcome := SupersedeNone
	if m.Supersedes != "" {
		outcome, err = supersedeWithinTx(ctx, tx, normalizeEmail(userEmail), m, now)
		if err != nil {
			return nil, SupersedeNone, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, SupersedeNone, err
	}
	return m, outcome, nil
}

// supersedeWithinTx applies the guarded retirement of accepted.Supersedes
// inside the accept transaction and returns the outcome label.
func supersedeWithinTx(ctx context.Context, tx *sql.Tx, userEmail string, accepted *Memory, now int64) (string, error) {
	var (
		content   string
		source    string
		pinned    bool
		retiredAt *int64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT content, source, pinned, retired_at FROM memories
		 WHERE id = $1 AND user_email = $2 FOR UPDATE`,
		accepted.Supersedes, userEmail,
	).Scan(&content, &source, &pinned, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SupersedeTargetMissing, nil
	}
	if err != nil {
		return SupersedeNone, err
	}
	switch {
	case retiredAt != nil:
		return SupersedeTargetRetired, nil
	case pinned:
		return SupersedeTargetPinned, nil
	case source == "proposed":
		return SupersedeTargetProposed, nil
	case MemoryContentHash(content) != accepted.SupersedesHash:
		return SupersedeTargetChanged, nil
	}
	// Guarded write: the WHERE re-checks liveness so a concurrent accept of a
	// second proposal against the same target cannot double-retire it.
	res, err := tx.ExecContext(ctx,
		`UPDATE memories SET retired_at = $1, retired_by = $2, updated_at = $1
		 WHERE id = $3 AND user_email = $4 AND retired_at IS NULL AND pinned = FALSE AND source != 'proposed'`,
		now, accepted.ID, accepted.Supersedes, userEmail,
	)
	if err != nil {
		return SupersedeNone, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return SupersedeTargetRetired, nil
	}
	return SupersedeRetired, nil
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
