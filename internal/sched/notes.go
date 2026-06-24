// Package sched is the orchestrator cluster root. It hosts the admin-curated
// agent-notes / wiki store (notes.go) that the unified runtime injects into the
// system prompt for BOTH modes. The scheduler / storage / handlers / models /
// apikeys / db subpackages make up the rest of the orchestrator.
//
// Notes live in the sched DB (migration 015). Agents cannot write notes
// directly; they propose edits via the propose_note tool and admins curate
// (publish / reject) them.
package sched

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
)

// ErrNoteNotFound is returned when a note or proposal lookup misses.
var ErrNoteNotFound = errors.New("note not found")

// ErrSlugConflict is returned when creating a note whose slug already exists.
var ErrSlugConflict = errors.New("note slug already exists")

// ErrInvalidSlug is returned when a slug fails the ^[a-z0-9_-]{1,128}$ rule.
var ErrInvalidSlug = errors.New("invalid slug: must match ^[a-z0-9_-]{1,128}$")

// ErrInvalidBody is returned when a body is not valid UTF-8 or exceeds 1 MiB.
var ErrInvalidBody = errors.New("invalid body: must be valid UTF-8 and <= 1 MiB")

const maxNoteBodyBytes = 1 << 20 // 1 MiB

var slugPattern = regexp.MustCompile(`^[a-z0-9_-]{1,128}$`)

// Note is an admin-curated knowledge-base entry.
type Note struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"` // "published" | "archived"
	CreatedBy string    `json:"created_by"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	Version   int       `json:"version"`
}

// NoteProposal is an agent-proposed edit awaiting admin curation.
type NoteProposal struct {
	ID           uuid.UUID  `json:"id"`
	NoteID       *uuid.UUID `json:"note_id"` // nil = create-new
	Slug         string     `json:"slug"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	Reason       string     `json:"reason"`
	Status       string     `json:"status"` // "pending" | "published" | "rejected"
	ProposedBy   string     `json:"proposed_by"`
	ProposedAt   int64      `json:"proposed_at"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    int64      `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
}

// Store is the notes/proposals data layer over the sched DB connection.
type Store struct {
	conn *sql.DB
}

// NewStore builds a notes Store over the sched Database.
func NewStore(database *db.Database) *Store {
	return &Store{conn: database.Conn()}
}

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return ErrInvalidSlug
	}
	return nil
}

func validateBody(body string) error {
	if !utf8.ValidString(body) || len(body) > maxNoteBodyBytes {
		return ErrInvalidBody
	}
	return nil
}

// ── notes CRUD (admin) ──

// CreateNote inserts a published note. Returns ErrSlugConflict if the slug is
// taken (or was previously used and archived — slug is globally UNIQUE).
func (s *Store) CreateNote(ctx context.Context, slug, title, body, createdBy string) (*Note, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	id := uuid.New()
	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO agent_notes (id, slug, title, body, status, created_by, updated_by, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, 'published', $5, $5, $6, $6, 1)`,
		id, slug, title, body, createdBy, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSlugConflict
		}
		return nil, err
	}
	return s.GetNote(ctx, id)
}

// UpdateNote updates a note's title and/or body (nil = leave unchanged) and
// bumps its version. The slug is immutable.
func (s *Store) UpdateNote(ctx context.Context, id uuid.UUID, title, body *string, updatedBy string) (*Note, error) {
	if body != nil {
		if err := validateBody(*body); err != nil {
			return nil, err
		}
	}
	now := time.Now().Unix()
	res, err := s.conn.ExecContext(ctx, `
		UPDATE agent_notes SET
			title = COALESCE($2, title),
			body = COALESCE($3, body),
			updated_by = $4,
			updated_at = $5,
			version = version + 1
		WHERE id = $1`,
		id, title, body, updatedBy, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNoteNotFound
	}
	return s.GetNote(ctx, id)
}

// GetNote fetches a note by ID.
func (s *Store) GetNote(ctx context.Context, id uuid.UUID) (*Note, error) {
	row := s.conn.QueryRowContext(ctx, noteSelect+" WHERE id = $1", id)
	return scanNote(row)
}

// GetNoteBySlug fetches a note by slug.
func (s *Store) GetNoteBySlug(ctx context.Context, slug string) (*Note, error) {
	row := s.conn.QueryRowContext(ctx, noteSelect+" WHERE slug = $1", slug)
	return scanNote(row)
}

// ListNotes returns published notes (and archived too when includeArchived),
// ordered by updated_at DESC. version DESC then slug ASC are deterministic
// tiebreaks so two notes updated within the same unix second (the timestamp
// resolution) still come back in a stable, meaningful order — the more-edited
// note first, then alphabetical — rather than Postgres heap order.
func (s *Store) ListNotes(ctx context.Context, includeArchived bool) ([]Note, error) {
	const order = " ORDER BY updated_at DESC, version DESC, slug ASC"
	q := noteSelect + " WHERE status = 'published'" + order
	if includeArchived {
		q = noteSelect + order
	}
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotes(rows)
}

// ArchiveNote soft-deletes a note (status='archived').
func (s *Store) ArchiveNote(ctx context.Context, id uuid.UUID, updatedBy string) error {
	now := time.Now().Unix()
	res, err := s.conn.ExecContext(ctx, `
		UPDATE agent_notes SET status = 'archived', updated_by = $2, updated_at = $3
		WHERE id = $1`,
		id, updatedBy, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoteNotFound
	}
	return nil
}

// ── injection read path (hot path, both modes) ──

// ListPublishedNotes returns status='published' notes ordered by updated_at DESC.
func (s *Store) ListPublishedNotes(ctx context.Context) ([]Note, error) {
	return s.ListNotes(ctx, false)
}

// ── proposals (agent proposes; admin curates) ──

// CreateProposal stages an agent-proposed note edit as pending. note_id is
// resolved from the slug: an existing note's id, or NULL for a create-new.
func (s *Store) CreateProposal(ctx context.Context, slug, title, body, reason, proposedBy string) (*NoteProposal, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	if err := validateBody(body); err != nil {
		return nil, err
	}

	var noteID *uuid.UUID
	if existing, err := s.GetNoteBySlug(ctx, slug); err == nil {
		noteID = &existing.ID
	} else if !errors.Is(err, ErrNoteNotFound) {
		return nil, err
	}

	now := time.Now().Unix()
	id := uuid.New()
	_, err := s.conn.ExecContext(ctx, `
		INSERT INTO agent_note_proposals (id, note_id, slug, title, body, reason, status, proposed_by, proposed_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8)`,
		id, noteID, slug, title, body, reason, proposedBy, now)
	if err != nil {
		return nil, err
	}
	return s.GetProposal(ctx, id)
}

// ListProposals returns proposals filtered by status ("" = all), newest first.
func (s *Store) ListProposals(ctx context.Context, status string) ([]NoteProposal, error) {
	q := proposalSelect + " ORDER BY proposed_at DESC"
	var args []any
	if status != "" {
		q = proposalSelect + " WHERE status = $1 ORDER BY proposed_at DESC"
		args = append(args, status)
	}
	rows, err := s.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProposals(rows)
}

// GetProposal fetches a proposal by ID.
func (s *Store) GetProposal(ctx context.Context, id uuid.UUID) (*NoteProposal, error) {
	row := s.conn.QueryRowContext(ctx, proposalSelect+" WHERE id = $1", id)
	return scanProposal(row)
}

// PublishProposal upserts the proposal's content into agent_notes (create if
// the slug is new, else update + bump version) AND marks the proposal published
// — all in one transaction.
func (s *Store) PublishProposal(ctx context.Context, id uuid.UUID, decidedBy, decisionNote string) (*Note, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	// Lock the proposal row.
	var (
		slug, title, body, status string
	)
	err = tx.QueryRowContext(ctx,
		"SELECT slug, title, body, status FROM agent_note_proposals WHERE id = $1 FOR UPDATE", id).
		Scan(&slug, &title, &body, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "pending" {
		return nil, fmt.Errorf("proposal %s is not pending (status=%s)", id, status)
	}

	now := time.Now().Unix()
	var noteID uuid.UUID

	// Upsert by slug: update + bump version if it exists, else create.
	err = tx.QueryRowContext(ctx, "SELECT id FROM agent_notes WHERE slug = $1 FOR UPDATE", slug).Scan(&noteID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		noteID = uuid.New()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_notes (id, slug, title, body, status, created_by, updated_by, created_at, updated_at, version)
			VALUES ($1, $2, $3, $4, 'published', $5, $5, $6, $6, 1)`,
			noteID, slug, title, body, decidedBy, now); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_notes SET title = $2, body = $3, status = 'published',
				updated_by = $4, updated_at = $5, version = version + 1
			WHERE id = $1`,
			noteID, title, body, decidedBy, now); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_note_proposals SET status = 'published', note_id = $2,
			decided_by = $3, decided_at = $4, decision_note = $5
		WHERE id = $1`,
		id, noteID, decidedBy, now, nullableStr(decisionNote)); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetNote(ctx, noteID)
}

// RejectProposal marks a pending proposal rejected with a reason, leaving
// agent_notes untouched.
func (s *Store) RejectProposal(ctx context.Context, id uuid.UUID, decidedBy, reason string) error {
	now := time.Now().Unix()
	res, err := s.conn.ExecContext(ctx, `
		UPDATE agent_note_proposals SET status = 'rejected',
			decided_by = $2, decided_at = $3, decision_note = $4
		WHERE id = $1 AND status = 'pending'`,
		id, decidedBy, now, nullableStr(reason))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoteNotFound
	}
	return nil
}

// ── scan helpers ──

const noteSelect = "SELECT id, slug, title, body, status, created_by, updated_by, created_at, updated_at, version FROM agent_notes"

func scanNote(row interface{ Scan(...any) error }) (*Note, error) {
	var n Note
	err := row.Scan(&n.ID, &n.Slug, &n.Title, &n.Body, &n.Status, &n.CreatedBy, &n.UpdatedBy, &n.CreatedAt, &n.UpdatedAt, &n.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func scanNotes(rows *sql.Rows) ([]Note, error) {
	out := make([]Note, 0)
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Slug, &n.Title, &n.Body, &n.Status, &n.CreatedBy, &n.UpdatedBy, &n.CreatedAt, &n.UpdatedAt, &n.Version); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const proposalSelect = "SELECT id, note_id, slug, title, body, COALESCE(reason, ''), status, proposed_by, proposed_at, COALESCE(decided_by, ''), COALESCE(decided_at, 0), COALESCE(decision_note, '') FROM agent_note_proposals"

func scanProposal(row interface{ Scan(...any) error }) (*NoteProposal, error) {
	p, err := scanProposalRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func scanProposals(rows *sql.Rows) ([]NoteProposal, error) {
	out := make([]NoteProposal, 0)
	for rows.Next() {
		p, err := scanProposalRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func scanProposalRow(row interface{ Scan(...any) error }) (*NoteProposal, error) {
	var p NoteProposal
	var noteID *uuid.UUID
	err := row.Scan(&p.ID, &noteID, &p.Slug, &p.Title, &p.Body, &p.Reason, &p.Status, &p.ProposedBy, &p.ProposedAt, &p.DecidedBy, &p.DecidedAt, &p.DecisionNote)
	if err != nil {
		return nil, err
	}
	p.NoteID = noteID
	return &p, nil
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isUniqueViolation(err error) bool {
	// 23505 is unique_violation. Match the typed pgconn error (pgconn is already
	// a transitive dep used by the db layer) rather than substring-scanning the
	// message, which can false-positive when "23505" appears in a note slug/body.
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
