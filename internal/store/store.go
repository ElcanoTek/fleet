// Package store is the Postgres-backed conversation store for chat-server.
//
// It owns the database connection pool and exposes the narrow CRUD surface
// that the HTTP handlers need. All conversation IDs are v4 UUIDs; all
// timestamps are unix seconds (int64).
//
// Retention: conversations with pinned=false and updated_at older than TTL
// are deleted by SweepExpired. A per-user cap further evicts the oldest
// unpinned conversations beyond UnpinnedCap. Pinned conversations are
// exempt from both.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	// Register pgx as the "pgx" database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lib/pq"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// Store wraps the Postgres handle. Schema is managed by the embedded
// migrations (see migrations.go + migrations/*.sql).
type Store struct {
	db *sql.DB
	// searchEnabled gates full-text search index maintenance (#308): when false,
	// AppendHistory skips writing message_search_content and the backfill is a
	// no-op, so a high-write deployment can opt out of GIN index upkeep
	// (FLEET_SEARCH_ENABLED=false). Defaults to true. Set via SetSearchEnabled.
	searchEnabled bool
	// softDelete gates soft-delete behavior (#279): when true, Delete / DeleteByIDs
	// / DeleteAllUnpinned set deleted_at = NOW() instead of issuing a hard DELETE,
	// and List / Get / search hide rows whose deleted_at is set. Defaults to false
	// (hard delete — no behavior change for existing deployments). Set via
	// SetSoftDelete from FLEET_CONVERSATION_SOFT_DELETE.
	softDelete bool
}

// SetSearchEnabled toggles full-text search index maintenance. cmd/fleet calls
// this from config (FLEET_SEARCH_ENABLED) right after Open. Off → AppendHistory
// stops populating message_search_content and BackfillSearchContent no-ops.
func (s *Store) SetSearchEnabled(enabled bool) { s.searchEnabled = enabled }

// SetSoftDelete toggles soft-delete mode. cmd/fleet calls this from config
// (FLEET_CONVERSATION_SOFT_DELETE) right after Open. On → delete operations
// tombstone rows via deleted_at and reads hide them; a 30-day sweeper (run
// inside SweepExpired) permanently removes expired tombstones. Off (default)
// → delete is a hard DELETE, unchanged from the historical behavior.
func (s *Store) SetSoftDelete(enabled bool) { s.softDelete = enabled }

// Conversation is the list-item shape exposed to handlers.
type Conversation struct {
	ID        string `json:"id"`
	UserEmail string `json:"user_email"`
	Title     string `json:"title"`
	Persona   string `json:"persona"`
	// Model is the per-chat OpenRouter slug override. Empty = use the
	// server-configured primary. Set via PUT /conversations/{id}/model.
	Model     string `json:"model"`
	Pinned    bool   `json:"pinned"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// OptionalMCPServersEnabled is the set of Optional MCP server names
	// the user has opted in for this conversation. Default-on servers
	// (sendgrid, email, fast_io) are NOT listed here — their tools are
	// always registered. Only tools from servers marked spec.Optional=true
	// are gated by this list. Stored as JSONB in Postgres; marshalled
	// as a JSON array over the wire. nil / empty = no opt-ins.
	OptionalMCPServersEnabled []string `json:"optional_mcp_servers_enabled"`
	// Lockdown is set at conversation creation and never changes. When
	// true: the per-turn sandbox is cold-started fresh with
	// --network=none and the model slug must be in
	// CHAT_LOCKDOWN_ALLOWED_MODELS. Non-lockdown chats also run in
	// containers (default mode), but reuse the warm pool and inherit
	// rootless slirp4netns. Drives the "Lockdown chat" badge on the
	// frontend.
	Lockdown bool `json:"lockdown"`
	// ArchivedAt is nil for active conversations and a unix timestamp
	// (seconds) for archived ones (#282). Archived conversations are hidden
	// from the default GET /conversations list but remain fully readable via
	// ?archived=true, and are excluded from the unpinned-cap eviction.
	ArchivedAt *int64 `json:"archived_at"`
	// TitleLocked is set when the user manually renames a conversation (#302).
	// While true, the background auto-titler skips it so a manual name is never
	// silently overwritten.
	TitleLocked bool `json:"title_locked"`
	// Folder is a free-form bucket name (e.g. "Archive", "Old work") used by
	// bulk move (#279). Empty string is the default "no folder" state. Set via
	// PATCH /conversations/bulk with changes.folder.
	Folder string `json:"folder,omitempty"`
	// Labels is a tag set for grouping/filtering (#279). Empty = unlabeled.
	// Set via PATCH /conversations/bulk with changes.labels.
	Labels []string `json:"labels,omitempty"`
	// DeletedAt is the soft-delete tombstone (#279). nil = live; non-nil unix
	// seconds = soft-deleted (hidden from GET /conversations and search). Only
	// ever set when FLEET_CONVERSATION_SOFT_DELETE=true; the default hard-delete
	// path removes the row outright so this stays nil.
	DeletedAt *int64 `json:"deleted_at,omitempty"`
	// ApprovalTimeoutSeconds overrides the global FLEET_APPROVAL_TIMEOUT_SECONDS
	// default-deny window for critical-tool approval cards in this conversation
	// (#225). nil = use the global default; a positive value sets a per-chat
	// override. Set via POST /conversations/{id}/approval-timeout.
	ApprovalTimeoutSeconds *int `json:"approval_timeout_seconds,omitempty"`
	// ShareToken is the opt-in public read-only share token (#226). Empty = not
	// shared; non-empty = anyone with /shared/{ShareToken} can view a read-only
	// snapshot. Surfaced to the owner's own GET /conversations so the sidebar can
	// show a "shared" badge and a copy-link action. Set/cleared via
	// POST/DELETE /conversations/{id}/share.
	ShareToken string `json:"share_token,omitempty"`
}

// ErrForeignConversation is returned by DeleteByIDs / BulkPatch when one or
// more of the supplied IDs do not belong to the caller (or do not exist). The
// HTTP layer surfaces it as 403 and the whole operation is a no-op.
var ErrForeignConversation = errors.New("one or more conversation IDs not owned by caller")

// ErrTitleLocked is returned by UpdateTitle when the conversation's title is
// locked by a manual rename (#302) — the auto-titler treats it as "skip", not a
// failure.
var ErrTitleLocked = errors.New("title is locked by a manual rename")

// PoolConfig tunes the chat DB connection pool (#276). Kept local to the store
// package (the cmd layer maps the env-derived config into it) so this low-level
// package stays decoupled from internal/config. DefaultPoolConfig reproduces the
// historical hard-coded settings.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultPoolConfig is the behavior-preserving baseline (used by tests and as a
// fallback): 25 open / 5 idle, 5m lifetime, 5s connect ping.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
}

// Open connects to Postgres using the given DSN (DATABASE_URL format or
// keyword/value — anything pgx accepts), applies the pool settings, and runs any
// pending migrations. Fails loudly if the DB is newer than the binary knows
// about (prevents accidental downgrade).
func Open(dsn string, pool PoolConfig) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("empty database DSN (set DATABASE_URL)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// Pool settings are operator-tunable (#276); defaults stay well under
	// Postgres' default max_connections=100 for a single-box deployment.
	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	connectTimeout := pool.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 5 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := applyMigrations(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, searchEnabled: true}, nil
}

// Close the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// TruncateAllForTest wipes every data row. Test-only helper — never
// call from production code. schema_migrations is preserved so Open()
// after a truncate is still a no-op on the second run.
func (s *Store) TruncateAllForTest(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`TRUNCATE TABLE conversations, memories, users, panic_events RESTART IDENTITY CASCADE`)
	return err
}

// RecordPanic appends a recovered-panic row (#241). Called best-effort from the
// safe.PanicEventWriter hook that cmd/fleet registers; failures are logged by the
// caller, never propagated into the recovery path.
func (s *Store) RecordPanic(ctx context.Context, location, message, stack string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO panic_events (ts, location, message, stack) VALUES ($1, $2, $3, $4)`,
		time.Now().Unix(), location, message, stack,
	)
	return err
}

// CountPanics returns the number of recorded panic events (test/diagnostic helper).
func (s *Store) CountPanics(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM panic_events`).Scan(&n)
	return n, err
}

// CreateConversation inserts a new conversation and returns its generated ID.
// model may be empty on creation — the frontend sends a slug with the first
// turn, which is then persisted via SetModel.
//
// lockdown is set once at creation. The frontend exposes this as a
// separate "New lockdown chat" affordance and the bit can never be
// mutated afterward — matches how persona is locked after the first
// turn.
func (s *Store) CreateConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool) (*Conversation, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	// optional_mcp_servers_enabled gets the column default ('[]'::jsonb);
	// we don't need to write it explicitly on insert.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, FALSE, $6, $7, $8)`,
		id, userEmail, title, persona, model, lockdown, now, now,
	)
	if err != nil {
		return nil, err
	}
	return &Conversation{
		ID:                        id,
		UserEmail:                 userEmail,
		Title:                     title,
		Persona:                   persona,
		Model:                     model,
		Pinned:                    false,
		Lockdown:                  lockdown,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		OptionalMCPServersEnabled: nil,
	}, nil
}

// SetOptionalMCPServers persists the user's opt-in list for this
// conversation. Callers MUST pass a normalised list (trimmed, deduped,
// lowercased, each name known to the running server). Stored as JSONB
// so we can round-trip via database/sql without pgtype plumbing.
//
// Empty list is a legal state — clears any prior opt-ins.
func (s *Store) SetOptionalMCPServers(ctx context.Context, userEmail, convID string, servers []string) error {
	if servers == nil {
		servers = []string{}
	}
	payload, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("marshal optional mcp servers: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET optional_mcp_servers_enabled = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4`,
		payload, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// scanOptionalMCPServers decodes the JSONB payload. Tolerant of NULL and
// malformed rows — both yield nil without erroring, because a read-path
// decode failure should never block the caller from seeing the rest of
// the conversation record. The error path is logged in the ops console
// only.
func scanOptionalMCPServers(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetModel updates the per-chat OpenRouter slug. Empty model clears the
// stored value; the frontend will supply its DEFAULT_MODEL on the next turn.
func (s *Store) SetModel(ctx context.Context, userEmail, convID, model string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET model = $1, updated_at = $2 WHERE id = $3 AND user_email = $4`,
		model, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// UpdateTitle is called after the first assistant reply (when we have enough
// context to auto-name the conversation).
// UpdateTitle sets the title from the AUTO-titler (#302). It is guarded by
// title_locked = FALSE so a user's manual rename is never overwritten; when the
// title is locked it makes no change and returns ErrTitleLocked, which the
// caller treats as a benign skip.
func (s *Store) UpdateTitle(ctx context.Context, userEmail, convID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND title_locked = FALSE`,
		title, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either the row is locked or it's gone; the auto-titler skips both.
		return ErrTitleLocked
	}
	return nil
}

// RenameTitle applies a MANUAL rename (#302): it sets the title and locks it
// (title_locked = TRUE) in one statement, unconditionally — a manual rename
// always wins and pins the name against the auto-titler thereafter.
func (s *Store) RenameTitle(ctx context.Context, userEmail, convID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = $1, title_locked = TRUE, updated_at = $2 WHERE id = $3 AND user_email = $4`,
		title, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// SetPinned toggles the pin state for a conversation.
func (s *Store) SetPinned(ctx context.Context, userEmail, convID string, pinned bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = $1, updated_at = $2 WHERE id = $3 AND user_email = $4`,
		pinned, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// SetArchived archives or unarchives a conversation (#282). archived=true sets
// archived_at = now; archived=false clears it (NULL). Archiving also clears the
// pin: "pinned" means keep-prominent, which is the opposite of filing away, so
// the two states are mutually exclusive (the issue's pinned-interaction rule).
func (s *Store) SetArchived(ctx context.Context, userEmail, convID string, archived bool) error {
	now := time.Now().Unix()
	var archivedAt any // NULL when unarchiving
	pinned := false    // archiving always unpins; unarchiving leaves it unpinned
	if archived {
		archivedAt = now
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET archived_at = $1, pinned = $2, updated_at = $3 WHERE id = $4 AND user_email = $5`,
		archivedAt, pinned, now, convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// nullableSeconds converts a scanned nullable INTEGER column into the *int the
// Conversation struct uses: NULL → nil ("use the global default"), present →
// a heap-allocated copy. Kept narrow so List/Get scan the per-conversation
// approval-timeout override identically (#225).
func nullableSeconds(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// SetApprovalTimeout sets (or clears) the per-conversation approval-timeout
// override (#225). seconds == nil clears it back to the global default; a
// non-nil pointer stores that many seconds. Callers validate the range at the
// HTTP layer; the store only persists. Scoped by user_email so a caller can
// only touch their own conversations.
func (s *Store) SetApprovalTimeout(ctx context.Context, userEmail, convID string, seconds *int) error {
	var arg any // NULL when clearing
	if seconds != nil {
		arg = *seconds
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET approval_timeout_seconds = $1, updated_at = $2 WHERE id = $3 AND user_email = $4`,
		arg, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// SharedConversation is the read-only public snapshot returned for a valid
// share token (#226). It deliberately omits id and user_email: an observer with
// the link learns the content but neither the internal ID nor who authored it.
type SharedConversation struct {
	Title     string               `json:"title"`
	Persona   string               `json:"persona"`
	Model     string               `json:"model"`
	CreatedAt int64                `json:"created_at"`
	SharedAt  int64                `json:"shared_at"`
	Messages  []agent.HistoryEntry `json:"messages"`
}

// SetShareToken (re)issues the public read-only share token for a conversation
// the caller owns (#226). Revoke-then-reissue: a second call rotates the token
// and resets shared_at. expiresAt is the optional unix-seconds expiry (nil =
// never expires). Scoped by user_email so a caller can only share their own
// conversation.
func (s *Store) SetShareToken(ctx context.Context, ownerEmail, convID, token string, expiresAt *int64) error {
	var expiresArg any // NULL when no expiry
	if expiresAt != nil {
		expiresArg = *expiresAt
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET share_token = $1, shared_at = $2, share_expires_at = $3
		 WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL`,
		token, time.Now().Unix(), expiresArg, convID, ownerEmail,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// RevokeShareToken clears the share token (and its metadata) for a conversation
// the caller owns (#226). Genuinely idempotent: revoking an already-unshared
// conversation matches the row but changes no columns (RowsAffected = 0), which
// is still success — so a double DELETE answers 204 both times rather than a
// spurious 500. Ownership is enforced by the WHERE clause AND by the handler's
// pre-check (which distinguishes a non-owned id as 404). deleted_at IS NULL
// matches SetShareToken: a soft-deleted conversation is not mutable.
func (s *Store) RevokeShareToken(ctx context.Context, ownerEmail, convID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET share_token = NULL, shared_at = NULL, share_expires_at = NULL
		 WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL`,
		convID, ownerEmail,
	)
	return err
}

// GetConversationByShareToken returns the read-only snapshot for a share token,
// or (nil, nil) when the token is unknown, revoked, or expired (#226). The
// lookup is NOT scoped by user — anyone with the token may read it — but expiry
// is enforced server-side here (now is unix seconds). It excludes soft-deleted
// conversations so a tombstoned chat can't be read through a stale link.
func (s *Store) GetConversationByShareToken(ctx context.Context, token string, now int64) (*SharedConversation, error) {
	if token == "" {
		return nil, nil
	}
	var (
		id  string
		out SharedConversation
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, persona, model, created_at, COALESCE(shared_at, 0)
		 FROM conversations
		 WHERE share_token = $1 AND deleted_at IS NULL
		   AND (share_expires_at IS NULL OR share_expires_at > $2)`,
		token, now,
	).Scan(&id, &out.Title, &out.Persona, &out.Model, &out.CreatedAt, &out.SharedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	msgs, err := s.LoadHistory(ctx, id)
	if err != nil {
		return nil, err
	}
	// Expose ONLY user/assistant text to the public snapshot. The full history
	// also carries tool_call / tool_result / reasoning entries whose content can
	// include command output, API responses, or other internals that were never
	// meant to be shared. Filtering here (not just in the UI) is the security
	// boundary: any consumer of this snapshot — including a raw JSON fetch —
	// sees the transcript, not the agent's working trace (#226).
	out.Messages = make([]agent.HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		if m.Type == "text" && (m.Role == "user" || m.Role == "assistant") {
			out.Messages = append(out.Messages, m)
		}
	}
	return &out, nil
}

// AutoArchiveOlderThan archives unpinned, not-already-archived conversations
// whose updated_at is older than d (#282). Returns the count archived. A zero or
// negative duration is a no-op (the feature is disabled). This is a softer
// alternative to the TTL hard-delete in SweepExpired — conversations are filed
// away rather than destroyed.
func (s *Store) AutoArchiveOlderThan(ctx context.Context, d time.Duration) (int, error) {
	if d <= 0 {
		return 0, nil
	}
	now := time.Now().Unix()
	cutoff := time.Now().Add(-d).Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET archived_at = $1, updated_at = $2
		 WHERE pinned = FALSE AND archived_at IS NULL AND updated_at < $3`,
		now, now, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("auto-archive: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Delete removes a conversation and (via FK cascade) its messages. When
// FLEET_CONVERSATION_SOFT_DELETE=true it instead tombstones the row
// (deleted_at = NOW()) so a future restore can undelete it; the hard DELETE
// is deferred to the 30-day sweeper in SweepExpired.
func (s *Store) Delete(ctx context.Context, userEmail, convID string) error {
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1 WHERE id = $2 AND user_email = $3 AND deleted_at IS NULL`,
			time.Now().Unix(), convID, userEmail,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return errors.New("conversation not found")
		}
		return nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = $1 AND user_email = $2`,
		convID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("conversation not found")
	}
	return nil
}

// DeleteAllUnpinned removes every unpinned conversation for a user. Pinned
// conversations — and archived ones (#282), which the user can't see when
// triggering this from the sidebar and which represent an intentional "keep"
// state — are untouched. Returns the count removed. In soft-delete mode it
// tombstones instead of hard-deleting.
func (s *Store) DeleteAllUnpinned(ctx context.Context, userEmail string) (int, error) {
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE user_email = $2 AND pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL`,
			time.Now().Unix(), userEmail,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE user_email = $1 AND pinned = FALSE AND archived_at IS NULL`,
		userEmail,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteByIDs removes the conversations identified by ids, scoped by ownership.
// Returns ErrForeignConversation (mapped to 403 by the HTTP layer) if any
// supplied ID is not owned by the caller or does not exist — in that case the
// whole operation is a no-op. In soft-delete mode it tombstones instead of
// hard-deleting. The caller is responsible for capping len(ids) (HTTP layer
// enforces 100).
func (s *Store) DeleteByIDs(ctx context.Context, userEmail string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// Ownership pre-check: every supplied ID must exist and belong to the
	// caller. A foreign or unknown ID aborts the whole request — never a
	// partial delete — matching the issue's "one foreign ID aborts the whole
	// request" policy.
	var owned int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM conversations WHERE id = ANY($1) AND user_email = $2 AND deleted_at IS NULL`,
		pq.Array(ids), userEmail,
	).Scan(&owned); err != nil {
		return 0, err
	}
	if owned != len(ids) {
		return 0, ErrForeignConversation
	}
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE id = ANY($2) AND user_email = $3 AND deleted_at IS NULL`,
			time.Now().Unix(), pq.Array(ids), userEmail,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = ANY($1) AND user_email = $2`,
		pq.Array(ids), userEmail,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteAllMatching removes (or, in soft-delete mode, tombstones) every
// conversation for the user matching the optional folder and label filters.
// An empty folder string is treated as "no folder filter"; pass a non-empty
// folder to target a bucket. An empty label is "no label filter". Returns the
// count affected.
//
// The query binds both filters as parameters with an `$n = ”` short-circuit so
// no SQL is concatenated from the inputs (defense against injection-by-clause).
func (s *Store) DeleteAllMatching(ctx context.Context, userEmail, folder, label string) (int, error) {
	now := time.Now().Unix()
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE user_email = $2 AND pinned = FALSE AND deleted_at IS NULL
			   AND ($3 = '' OR folder = $3)
			   AND ($4 = '' OR $4 = ANY(labels))`,
			now, userEmail, folder, label,
		)
		if err != nil {
			return 0, err
		}
		c, _ := res.RowsAffected()
		return int(c), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations
		 WHERE user_email = $1 AND pinned = FALSE AND deleted_at IS NULL
		   AND ($2 = '' OR folder = $2)
		   AND ($3 = '' OR $3 = ANY(labels))`,
		userEmail, folder, label,
	)
	if err != nil {
		return 0, err
	}
	c, _ := res.RowsAffected()
	return int(c), nil
}

// BulkPatch applies the supplied mutations to the conversations identified by
// ids in a single transaction. A nil pointer (pinned / folder / labels) means
// "leave that field untouched"; a non-nil pointer — including an empty labels
// slice — overwrites the stored value. Returns ErrForeignConversation (mapped to
// 403) if any supplied ID is foreign or unknown; the transaction rolls back so
// nothing is mutated. The caller caps len(ids) (HTTP layer enforces 100).
func (s *Store) BulkPatch(ctx context.Context, userEmail string, ids []string, pinned *bool, folder *string, labels []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Ownership pre-check: count rows the caller owns (and that are live).
	var owned int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM conversations WHERE id = ANY($1) AND user_email = $2 AND deleted_at IS NULL`,
		pq.Array(ids), userEmail,
	).Scan(&owned); err != nil {
		return 0, err
	}
	if owned != len(ids) {
		return 0, ErrForeignConversation
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE conversations
         SET pinned     = COALESCE($3, pinned),
             folder     = COALESCE($4, folder),
             labels     = COALESCE($5, labels),
             updated_at = $6
         WHERE id = ANY($1) AND user_email = $2 AND deleted_at IS NULL`,
		pq.Array(ids), userEmail,
		pinned, folder, pq.Array(labels),
		time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// List returns the user's conversations, pinned first, newest first. When
// archivedOnly is false it returns only active (archived_at IS NULL)
// conversations — the default sidebar view; when true it returns only the
// archived ones (#282). The two are distinct lists so the frontend can render
// archived conversations in a separate, collapsed section.
func (s *Store) List(ctx context.Context, userEmail string, archivedOnly bool) ([]Conversation, error) {
	archivedFilter := "archived_at IS NULL"
	if archivedOnly {
		archivedFilter = "archived_at IS NOT NULL"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at, archived_at, title_locked, optional_mcp_servers_enabled, folder, labels, approval_timeout_seconds, COALESCE(share_token, '')
		 FROM conversations WHERE user_email = $1 AND `+archivedFilter+` AND deleted_at IS NULL
		 ORDER BY pinned DESC, updated_at DESC, id DESC`,
		userEmail,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		var optionalRaw []byte
		var approvalTimeout sql.NullInt64
		if err := rows.Scan(&c.ID, &c.UserEmail, &c.Title, &c.Persona, &c.Model, &c.Pinned, &c.Lockdown, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.TitleLocked, &optionalRaw, &c.Folder, pq.Array(&c.Labels), &approvalTimeout, &c.ShareToken); err != nil {
			return nil, err
		}
		c.OptionalMCPServersEnabled = scanOptionalMCPServers(optionalRaw)
		c.ApprovalTimeoutSeconds = nullableSeconds(approvalTimeout)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get fetches a single conversation (without messages).
func (s *Store) Get(ctx context.Context, userEmail, convID string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at, archived_at, title_locked, optional_mcp_servers_enabled, folder, labels, approval_timeout_seconds, COALESCE(share_token, '')
		 FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL`,
		convID, userEmail,
	)
	var c Conversation
	var optionalRaw []byte
	var approvalTimeout sql.NullInt64
	if err := row.Scan(&c.ID, &c.UserEmail, &c.Title, &c.Persona, &c.Model, &c.Pinned, &c.Lockdown, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.TitleLocked, &optionalRaw, &c.Folder, pq.Array(&c.Labels), &approvalTimeout, &c.ShareToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c.OptionalMCPServersEnabled = scanOptionalMCPServers(optionalRaw)
	c.ApprovalTimeoutSeconds = nullableSeconds(approvalTimeout)
	return &c, nil
}

// LoadHistory returns every stored message event for a conversation in
// insertion order.
func (s *Store) LoadHistory(ctx context.Context, convID string) ([]agent.HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, type, content FROM messages WHERE conversation_id = $1 ORDER BY id ASC`,
		convID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []agent.HistoryEntry
	for rows.Next() {
		var e agent.HistoryEntry
		var content string
		if err := rows.Scan(&e.Role, &e.Type, &content); err != nil {
			return nil, err
		}
		e.Content = json.RawMessage(content)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendHistory writes every entry in turn order and bumps the conversation
// updated_at. Done inside a single transaction so partial writes don't leave
// torn state if the process dies mid-turn.
func (s *Store) AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	var b strings.Builder
	b.WriteString(`INSERT INTO messages (conversation_id, role, type, content, created_at) VALUES `)
	args := make([]any, 0, len(entries)*5)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*5 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d)", base, base+1, base+2, base+3, base+4)
		args = append(args, convID, e.Role, e.Type, string(e.Content), now)
	}
	// RETURNING id (in VALUES order) so we can link the extracted FTS plaintext
	// rows back to their messages. Postgres preserves multi-row INSERT order.
	b.WriteString(" RETURNING id")
	ids := make([]int64, 0, len(entries))
	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	// Close before the next statement on this tx (one active result set at a time).
	_ = rows.Close()
	if len(ids) != len(entries) {
		return fmt.Errorf("AppendHistory: inserted %d messages but got %d ids", len(entries), len(ids))
	}

	// Full-text search index maintenance (#308): extract searchable plaintext from
	// the just-inserted messages into message_search_content, in the same tx.
	if s.searchEnabled {
		if err := insertSearchContent(ctx, tx, convID, now, entries, ids); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, now, convID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceSummary deletes any prior `summary` messages on the conversation
// and inserts the new one in a single transaction. Replace semantics keep
// the user-initiated "summarize and continue" flow from chaining
// summary-of-summary as the user re-summarizes the same chat — every
// summarize call is one round-trip deep against the live history.
//
// Scoped by user_email: a foreign-owned conversation returns an error
// instead of mutating someone else's chat.
func (s *Store) ReplaceSummary(ctx context.Context, userEmail, convID string, entry agent.HistoryEntry) error {
	if entry.Type != "summary" {
		return fmt.Errorf("ReplaceSummary: entry type must be \"summary\", got %q", entry.Type)
	}
	owned, err := s.Get(ctx, userEmail, convID)
	if err != nil {
		return err
	}
	if owned == nil {
		return errors.New("conversation not found")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM messages WHERE conversation_id = $1 AND type = 'summary'`,
		convID,
	); err != nil {
		return fmt.Errorf("delete prior summary: %w", err)
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (conversation_id, role, type, content, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		convID, entry.Role, entry.Type, string(entry.Content), now,
	); err != nil {
		return fmt.Errorf("insert summary: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, now, convID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// TruncateAfter deletes every message in a conversation whose id is strictly
// greater than afterMessageID. Used by the retry + regenerate flows to clip
// off a cancelled/failed assistant tail before re-running the turn.
//
// Scoped by user_email: if the conversation belongs to someone else we
// return a 0-row error so the handler surfaces a 404.
func (s *Store) TruncateAfter(ctx context.Context, userEmail, convID string, afterMessageID int64) error {
	// Confirm ownership first; cheap row-level scope check.
	owned, err := s.Get(ctx, userEmail, convID)
	if err != nil {
		return err
	}
	if owned == nil {
		return errors.New("conversation not found")
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE conversation_id = $1 AND id > $2`,
		convID, afterMessageID,
	)
	if err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	// Bump updated_at so the sidebar reflects the change.
	_, err = s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`,
		time.Now().Unix(), convID,
	)
	return err
}

// Approval is a pending high-risk tool call awaiting user consent.
type Approval struct {
	ID             string
	ConversationID string
	UserEmail      string
	ToolName       string
	ArgsJSON       string
	Status         string // pending|approved|rejected
	ResultText     string
	CreatedAt      int64
	ResolvedAt     int64
	// ToolCallID is the id the agent assigned to the tool_call event
	// in the conversation history. Populated when the orchestration
	// layer stages the call; empty for older rows. The post-approval
	// resolver uses this to write the real tool_result back under the
	// same id the chip is keyed on, so the UI updates instead of
	// orphaning the result row.
	ToolCallID string
	// ExpiresAt is the unix-seconds deadline after which a still-pending
	// approval is auto-denied by the server-side expiry sweep — the
	// default-DENY-on-timeout contract for the web approval path (#225).
	// 0 means "no expiry" (legacy rows, or a non-positive resolved timeout);
	// the sweep and the UI countdown both treat 0 as "never expires".
	ExpiresAt int64
}

// ListPendingApprovals returns every pending approval for a conversation,
// oldest first. Used on page reload to re-render approval cards that were
// staged but never resolved in the previous browser session.
func (s *Store) ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0)
		 FROM approvals
		 WHERE conversation_id = $1 AND user_email = $2 AND status = 'pending'
		 ORDER BY created_at ASC`,
		convID, userEmail,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
			&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateApproval stages a pending approval and returns the row.
// toolCallID is the agent-assigned id of the tool_call event being
// staged; empty is allowed (older code paths) but populating it lets
// the post-approval resolver write its tool_result back under the same
// id the UI chip is keyed on.
// expiresAt is the unix-seconds default-deny deadline for the staged approval
// (#225); pass 0 for "no expiry" (the column is stored NULL and the server-side
// expiry sweep skips the row).
func (s *Store) CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string, expiresAt int64) (*Approval, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	var expiresArg any // NULL when there is no timeout
	if expiresAt > 0 {
		expiresArg = expiresAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (id, conversation_id, user_email, tool_name, tool_call_id, args_json, status, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8)`,
		id, convID, userEmail, toolName, toolCallID, argsJSON, now, expiresArg,
	)
	if err != nil {
		return nil, err
	}
	return &Approval{
		ID: id, ConversationID: convID, UserEmail: userEmail,
		ToolName: toolName, ToolCallID: toolCallID, ArgsJSON: argsJSON, Status: approvalStatusPending,
		CreatedAt: now, ExpiresAt: expiresAt,
	}, nil
}

// GetApproval looks up a pending approval, scoped by user_email.
func (s *Store) GetApproval(ctx context.Context, userEmail, approvalID string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0)
		 FROM approvals WHERE id = $1 AND user_email = $2`,
		approvalID, userEmail,
	)
	var a Approval
	if err := row.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
		&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// MaxExpiredApprovalsPerSweep bounds a single expiry-sweep batch so one sweep
// can't load an unbounded backlog into memory. A sweep that hits the cap logs
// and the next tick picks up the remainder (#225). Exported so the httpapi
// sweep can detect a full batch from a single source of truth.
const MaxExpiredApprovalsPerSweep = 500

// ListExpiredApprovals returns pending approvals whose expires_at deadline has
// passed (expires_at > 0 AND < now), oldest-deadline first, across ALL users
// and conversations. This is the read half of the server-side expiry sweep; the
// caller atomically claims and auto-denies each row (default-DENY-on-timeout).
// Rows with NULL/0 expires_at never expire and are excluded.
func (s *Store) ListExpiredApprovals(ctx context.Context, now int64) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0)
		 FROM approvals
		 WHERE status = 'pending' AND expires_at IS NOT NULL AND expires_at > 0 AND expires_at < $1
		 ORDER BY expires_at ASC
		 LIMIT $2`,
		now, MaxExpiredApprovalsPerSweep,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
			&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Approval lifecycle statuses.
const (
	approvalStatusPending  = "pending"
	approvalStatusApproved = "approved"
	approvalStatusRejected = "rejected"
)

func validApprovalResolution(status string) bool {
	return status == approvalStatusApproved || status == approvalStatusRejected
}

// ResolveApproval marks the approval approved or rejected and records the
// tool result text. Safe to call twice — second write is no-op via guard.
func (s *Store) ResolveApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) error {
	if !validApprovalResolution(newStatus) {
		return fmt.Errorf("invalid approval status %q", newStatus)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'`,
		newStatus, resultText, time.Now().Unix(), approvalID, userEmail,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("approval not found or already resolved")
	}
	return nil
}

// ClaimApproval atomically transitions a pending approval to newStatus
// and reports whether this caller won the claim. The staged tool must
// only be fired by the winner — two concurrent approve requests (a
// double-click, a mobile retry, two open tabs) would otherwise both
// pass an in-memory "still pending" check and both send the email.
func (s *Store) ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error) {
	if !validApprovalResolution(newStatus) {
		return false, fmt.Errorf("invalid approval status %q", newStatus)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'`,
		newStatus, resultText, time.Now().Unix(), approvalID, userEmail,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetApprovalResult records the staged tool's outcome on an
// already-claimed (non-pending) approval.
func (s *Store) SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET result_text = $1
		 WHERE id = $2 AND user_email = $3 AND status <> 'pending'`,
		resultText, approvalID, userEmail,
	)
	return err
}

// LatestApprovalByTool returns the most recent approval (any status)
// for a (conversation, tool) pair, or (nil, nil) if none exists. The
// suggest_advanced_model gate uses this to look up the prior card's
// disposition: an approved row stops re-suggestions for the rest of
// the conversation; a rejected row triggers a user-turn cooldown.
func (s *Store) LatestApprovalByTool(ctx context.Context, convID, toolName string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0)
		 FROM approvals
		 WHERE conversation_id = $1 AND tool_name = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		convID, toolName,
	)
	var a Approval
	if err := row.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
		&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// CountUserMessagesAfterTimestamp returns the number of user-role
// messages in a conversation whose created_at is strictly greater than
// ts. Used by the suggest_advanced_model gate to enforce a
// "re-suggest after N user turns" cooldown — counting user-role text
// messages reflects actual user-driven turns rather than tool/assistant
// chatter inside a single turn.
func (s *Store) CountUserMessagesAfterTimestamp(ctx context.Context, convID string, ts int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		 WHERE conversation_id = $1 AND role = 'user' AND type = 'text' AND created_at > $2`,
		convID, ts,
	).Scan(&n)
	return n, err
}

// SupersedePendingApprovals marks every pending approval for a
// (conversation, tool) pair as rejected, with a canned result text
// explaining it was superseded. Used when the agent stages a fresh
// approval for the same tool — e.g. retrying a preview_email after
// the first call contained garbage. Without this the UI accumulates
// stacked cards and the user has to dismiss each one manually.
// Returns the number of rows updated so the caller can decide
// whether to log or inject a history entry.
func (s *Store) SupersedePendingApprovals(ctx context.Context, convID, toolName string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals
		   SET status = 'rejected',
		       result_text = 'Superseded by a newer call to this tool.',
		       resolved_at = $1
		 WHERE conversation_id = $2
		   AND tool_name = $3
		   AND status = 'pending'`,
		time.Now().Unix(), convID, toolName,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MaxMessageIDForRole returns the DB row id of the latest message for this
// conversation that matches role. Used by the frontend's retry flow, which
// references messages by their UI-side id (a monotonically increasing
// timestamp) but ultimately needs the DB id to truncate against.
func (s *Store) MaxMessageIDForRole(ctx context.Context, convID, role string) (int64, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM messages WHERE conversation_id = $1 AND role = $2`,
		convID, role,
	)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// SecondMaxMessageIDForRole returns the DB row id of the SECOND-to-last
// message for this conversation + role. Used by the edit flow: to replace
// the latest user message, we truncate after the user BEFORE it (if any)
// so both the old user text and its assistant tail are removed.
func (s *Store) SecondMaxMessageIDForRole(ctx context.Context, convID, role string) (int64, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id FROM messages
		 WHERE conversation_id = $1 AND role = $2
		 ORDER BY id DESC LIMIT 1 OFFSET 1`,
		convID, role,
	)
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// TurnMetric is a single completed-turn row for the admin dashboard.
type TurnMetric struct {
	ConversationID      string
	UserEmail           string
	CompletedAt         int64
	CostUSD             float64
	PromptTokens        int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	Cancelled           bool
}

// RecordTurn writes a turn_metrics row. Called once per completed turn
// (success or cancelled). Failures are logged but not propagated — a
// missing metric row shouldn't kill a conversation.
func (s *Store) RecordTurn(ctx context.Context, m TurnMetric) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turn_metrics
		   (conversation_id, user_email, completed_at, cost_usd,
		    prompt_tokens, completion_tokens, cached_tokens, cache_creation_tokens, cancelled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		m.ConversationID, m.UserEmail, m.CompletedAt,
		m.CostUSD, m.PromptTokens, m.CompletionTokens, m.CachedTokens, m.CacheCreationTokens, m.Cancelled,
	)
	return err
}

// AdminRow is one user's aggregated stats for the admin dashboard.
type AdminRow struct {
	Email                    string
	ConversationCount        int
	PinnedCount              int
	LastActivity             int64
	TotalCostUSD             float64
	TotalTurns               int
	TotalPromptTokens        int64
	TotalCachedTokens        int64
	TotalCacheCreationTokens int64
}

// AdminStats aggregates per-user metrics for the /admin page. One query
// per section keeps the code simple; 10-20 users at chat scale means the
// whole thing returns in milliseconds.
func (s *Store) AdminStats(ctx context.Context) ([]AdminRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT
		    c.user_email                                 AS email,
		    COUNT(c.id)                                  AS conv_count,
		    SUM(CASE WHEN c.pinned THEN 1 ELSE 0 END)    AS pinned_count,
		    MAX(c.updated_at)                            AS last_activity
		 FROM conversations c
		 GROUP BY c.user_email`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byEmail := map[string]*AdminRow{}
	for rows.Next() {
		var r AdminRow
		if err := rows.Scan(&r.Email, &r.ConversationCount, &r.PinnedCount, &r.LastActivity); err != nil {
			return nil, err
		}
		row := r
		byEmail[r.Email] = &row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Cost + turn counts from turn_metrics. Left-joining inside a single
	// query would work too, but this is tidier and still 2 queries.
	metricRows, err := s.db.QueryContext(ctx,
		`SELECT user_email,
		        COALESCE(SUM(cost_usd), 0),
		        COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        COALESCE(SUM(cache_creation_tokens), 0)
		 FROM turn_metrics
		 GROUP BY user_email`,
	)
	if err != nil {
		return nil, err
	}
	defer metricRows.Close()
	for metricRows.Next() {
		var email string
		var cost float64
		var turns int
		var promptTokens, cachedTokens, cacheCreationTokens int64
		if err := metricRows.Scan(&email, &cost, &turns, &promptTokens, &cachedTokens, &cacheCreationTokens); err != nil {
			return nil, err
		}
		if row, ok := byEmail[email]; ok {
			row.TotalCostUSD = cost
			row.TotalTurns = turns
			row.TotalPromptTokens = promptTokens
			row.TotalCachedTokens = cachedTokens
			row.TotalCacheCreationTokens = cacheCreationTokens
		} else {
			byEmail[email] = &AdminRow{
				Email: email, TotalCostUSD: cost, TotalTurns: turns,
				TotalPromptTokens: promptTokens, TotalCachedTokens: cachedTokens,
				TotalCacheCreationTokens: cacheCreationTokens,
			}
		}
	}
	if err := metricRows.Err(); err != nil {
		return nil, err
	}

	out := make([]AdminRow, 0, len(byEmail))
	for _, r := range byEmail {
		out = append(out, *r)
	}
	// Most-recently-active first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity > out[j].LastActivity
	})
	return out, nil
}

// SweepExpired deletes unpinned conversations older than ttl and enforces
// unpinnedCap per user. Returns counts (for logging) and any error.
//
// Called at server startup and after every successful turn.
//
// Soft-delete (#279): when enabled, the TTL sweep tombstones rows
// (deleted_at = NOW()) instead of hard-deleting, and an additional 30-day
// purge step permanently removes rows whose deleted_at fell out of window —
// the deferred half of the soft-delete contract. The per-user cap path still
// hard-evicts (cap overflow is an operator-set retention limit, not a user
// action), and skips already-tombstoned rows so the count stays honest.
func (s *Store) SweepExpired(ctx context.Context, ttl time.Duration, unpinnedCap int) (expired int, evicted int, err error) {
	cutoff := time.Now().Add(-ttl).Unix()

	// Archived conversations (#282) are exempt from both cleanup paths, just
	// like pinned ones: archiving is a user-intentional "keep, but decluttered"
	// state, so it must not be hard-deleted by the TTL or evicted by the cap.

	// 0. Soft-delete tombstone purge (#279): permanently remove rows soft-deleted
	//    more than 30 days ago. Runs regardless of the soft-delete flag so a
	//    deployment that toggles it off still reaps any prior tombstones; a no-op
	//    when no rows match. Counted in `expired` so the log line reflects total
	//    reaped rows.
	purgeRes, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '30 days'`,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("soft-delete purge: %w", err)
	}
	pn, _ := purgeRes.RowsAffected()
	expired += int(pn)

	// 1. TTL sweep.
	if s.softDelete {
		// Tombstone instead of hard-delete: the row survives for the 30-day
		// restore window. Only touches live rows (deleted_at IS NULL) so a
		// re-sweep never re-tombstones.
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND updated_at < $2`,
			time.Now().Unix(), cutoff,
		)
		if err != nil {
			return expired, 0, fmt.Errorf("ttl sweep: %w", err)
		}
		n, _ := res.RowsAffected()
		expired += int(n)
	} else {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM conversations WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND updated_at < $1`,
			cutoff,
		)
		if err != nil {
			return expired, 0, fmt.Errorf("ttl sweep: %w", err)
		}
		n, _ := res.RowsAffected()
		expired += int(n)
	}

	// 2. Per-user cap. Find user emails that have >unpinnedCap unpinned,
	//    non-archived, live rows.
	if unpinnedCap <= 0 {
		return expired, 0, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_email, COUNT(*) FROM conversations
		 WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL GROUP BY user_email HAVING COUNT(*) > $1`,
		unpinnedCap,
	)
	if err != nil {
		return expired, 0, fmt.Errorf("cap scan: %w", err)
	}
	var overflowUsers []string
	for rows.Next() {
		var email string
		var count int
		if err := rows.Scan(&email, &count); err != nil {
			_ = rows.Close()
			return expired, 0, err
		}
		overflowUsers = append(overflowUsers, email)
	}
	_ = rows.Close()

	for _, email := range overflowUsers {
		// OFFSET without LIMIT: skip the N most-recent unpinned rows
		// and delete everything older. Postgres accepts a bare OFFSET
		// where SQLite required `LIMIT -1 OFFSET N`.
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM conversations WHERE id IN (
			    SELECT id FROM conversations
			    WHERE user_email = $1 AND pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL
			    ORDER BY updated_at DESC, id DESC
			    OFFSET $2
			 )`,
			email, unpinnedCap,
		)
		if err != nil {
			return expired, evicted, fmt.Errorf("cap evict for %s: %w", email, err)
		}
		n, _ := res.RowsAffected()
		evicted += int(n)
	}
	return expired, evicted, nil
}
