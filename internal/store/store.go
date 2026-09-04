// Package store is the Postgres-backed conversation store for chat-server.
//
// It owns the database connection pool and exposes the narrow CRUD surface
// that the HTTP handlers need. All conversation IDs are v4 UUIDs; all
// timestamps are unix seconds (int64).
//
// Retention: conversations with pinned=false and updated_at older than TTL
// are deleted by SweepExpired. A per-user cap further evicts the oldest
// unpinned conversations beyond UnpinnedCap. Pinned, archived, shared, and
// project-bound conversations are exempt from both.
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	// Register pgx as the "pgx" database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// maxBatchRows caps how many rows one multi-row INSERT carries in this
// package's batched writers. With the widest batched row (well under a dozen
// parameters) a chunk stays far below Postgres' 65535-parameter statement
// limit, so a caller-sized list can never overflow the statement.
const maxBatchRows = 500

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
	// tokenCipher encrypts the per-user remote-MCP OAuth secrets at rest (#443).
	// nil = the remote-MCP-OAuth feature is disabled (no FLEET_MCP_OAUTH_ENCRYPTION_KEY);
	// the token CRUD then fails closed via secretbox.ErrNoCipher rather than
	// storing plaintext. Set via SetTokenCipher right after Open.
	tokenCipher *secretbox.Cipher
}

// SetTokenCipher installs the AES-256-GCM cipher used to encrypt per-user
// remote-MCP OAuth secrets at rest (#443). cmd/fleet calls this from config
// (FLEET_MCP_OAUTH_ENCRYPTION_KEY) right after Open. A nil cipher leaves the
// feature disabled — the token CRUD fails closed rather than persisting
// plaintext secrets.
func (s *Store) SetTokenCipher(c *secretbox.Cipher) { s.tokenCipher = c }

// RemoteMCPEncryptionEnabled reports whether a token cipher is configured, i.e.
// whether the remote-MCP-OAuth feature can store secrets. Handlers gate on this
// to return an actionable "set FLEET_MCP_OAUTH_ENCRYPTION_KEY" error.
func (s *Store) RemoteMCPEncryptionEnabled() bool { return s.tokenCipher != nil }

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
	// MCPAccounts is this conversation's per-connector credential-seat
	// override (#988): server name → account label. A missing key means the
	// user's default seat (connections-page default_account for bundled
	// connectors; the is_default hosted seat for remote ones). Stored as
	// JSONB; nil/empty = no overrides.
	MCPAccounts map[string]string `json:"mcp_accounts,omitempty"`
	// ProjectID scopes this conversation to a project/space (#509). Set at
	// creation or re-filed later via SetConversationProject; empty = no
	// project. The turn path uses it to inject the project's standing
	// instructions + shared memory. Connector/persona/model inheritance
	// happens only at creation — re-filing does not retro-apply them.
	ProjectID string `json:"project_id,omitempty"`
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
	// ParentConversationID is set when this conversation is a BRANCH forked from
	// another (#454): it copied the parent's messages up to BranchPointMessageID,
	// then diverged. Empty = not a branch. Lineage metadata only — the branch is
	// independent, so the parent may be deleted without affecting it.
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	// BranchPointMessageID is the parent message id this conversation was forked
	// at (#454). 0 = not a branch.
	BranchPointMessageID int64 `json:"branch_point_message_id,omitempty"`
	// ThinkingConfig is the per-conversation Claude extended-thinking override
	// (#220). nil = inherit the global FLEET_DEFAULT_THINKING_BUDGET_TOKENS
	// default; non-nil sets an explicit per-chat choice (Enabled=false force-
	// disables even when a global default is set). Set via
	// PUT /conversations/{id}/thinking_config. Stored as nullable JSONB.
	ThinkingConfig *ThinkingConfig `json:"thinking_config,omitempty"`
	// TeamVisible is the owner's per-chat opt-in to read-only visibility for
	// their team (ADR-0013, surfaced by ADR-0057). false = private, the
	// default and the only state a chat outside a team-shared project can be
	// in. Reported on the owner's own listings so the rail can badge a
	// team-shared chat distinctly from a link-shared one — the two audiences
	// are different and a single unlabeled icon conflated them. Set via
	// POST /conversations/{id}/share-with-team.
	TeamVisible bool `json:"team_visible,omitempty"`
}

// ThinkingConfig is the persisted shape of a conversation's extended-thinking
// setting (#220). It mirrors agentcore.ThinkingConfig field-for-field; the chat
// turn-setup boundary maps between the two so the store stays free of the heavy
// agentcore dependency. BudgetTokens is validated at the HTTP layer (0 or
// [1024, 100000]) and clamped again by the producer.
type ThinkingConfig struct {
	Enabled      bool `json:"enabled"`
	BudgetTokens int  `json:"budget_tokens,omitempty"`
}

// ErrForeignConversation is returned by DeleteByIDs / BulkPatch when one or
// more of the supplied IDs do not belong to the caller (or do not exist). The
// HTTP layer surfaces it as 403 and the whole operation is a no-op.
var ErrForeignConversation = errors.New("one or more conversation IDs not owned by caller")

// ErrTitleLocked is returned by UpdateTitle when the conversation's title is
// locked by a manual rename (#302) — the auto-titler treats it as "skip", not a
// failure.
var ErrTitleLocked = errors.New("title is locked by a manual rename")

// ErrBranchPointNotFound is returned by BranchConversation when the requested
// branch-point message id names no message in the parent conversation (#454) —
// a client error (bad message id), not a server fault.
var ErrBranchPointNotFound = errors.New("branch point message not found in conversation")

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
//
// Every table must be reachable from this list via an FK CASCADE or named
// explicitly. turn_metrics is named because migration 038 deliberately
// dropped its conversations FK (usage history outlives conversation
// deletion), so it stopped cascading — which quietly made the usage-analytics
// tests non-rerunnable (rows accumulated across suite runs). projects,
// user_connector_prefs, and user_skills have no FK into any truncated table
// and are named for the same reason.
// The lock discipline below exists because a bare TRUNCATE here deadlocked in
// CI (SQLSTATE 40P01), failing PRs whose diffs touched nothing nearby. TRUNCATE
// takes ACCESS EXCLUSIVE on every listed table plus everything CASCADE reaches,
// and it takes them one at a time in list order — conversations first, users
// several entries later. An ordinary transaction that writes users and then
// conversations holds its row locks in the opposite order, so the two form a
// cycle and Postgres shoots one of them. The writers are real: a finished test's
// turn goroutines can still be draining while the next test's fixture wipes the
// database, which is exactly the window CI hit.
//
// Two mechanisms, because they answer different halves of the problem:
//
//   - The advisory lock serializes fixtures against each OTHER. It is taken
//     before any table lock, so a waiter holds nothing and truncate-vs-truncate
//     cannot cycle. Ordinary writers never take it, so it adds no new cycle.
//   - The retry handles fixture-vs-WRITER cycles, which no lock ordering here
//     can prevent — production writers are not going to coordinate with a test
//     helper. lock_timeout keeps a truncate stuck behind a long writer from
//     hanging the suite instead of retrying.
//
// This makes the fixture robust against transient contention, which is the
// failure CI saw. It is not a cure for a permanent writer: a goroutine that
// writes forever will still exhaust the attempts, and should — that is a leaked
// test goroutine worth failing on rather than hiding behind an infinite retry.
func (s *Store) TruncateAllForTest(ctx context.Context) error {
	// The first several attempts refuse to wait; the rest take their place in the
	// lock queue. See truncateAllForTestOnce.
	const maxAttempts = 14
	const nowaitAttempts = 6
	backoff := 5 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = s.truncateAllForTestOnce(ctx, attempt > nowaitAttempts)
		if lastErr == nil {
			return nil
		}
		// A schema or permission error will not improve on the next try.
		if !isRetryableLockError(lastErr) {
			return lastErr
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("truncate for test: %w", errors.Join(ctx.Err(), lastErr))
		case <-time.After(backoff):
		}
		if backoff < 250*time.Millisecond {
			backoff *= 2
		}
	}
	return fmt.Errorf("truncate for test: still contended after %d attempts, "+
		"which usually means a previous test leaked a goroutine that is still "+
		"writing: %w", maxAttempts, lastErr)
}

// truncateAllForTestOnce is one attempt, in its own transaction so a failed
// TRUNCATE leaves nothing half-applied and drops the advisory lock on rollback.
//
// queue selects between the two failure modes of a lock request, which starve in
// opposite directions. NOWAIT cannot deadlock but cannot win against a steady
// stream of writers either — it asks at one instant and is refused. A waiting
// request does the opposite: Postgres queues it, and because a pending ACCESS
// EXCLUSIVE blocks the row-level requests that arrive behind it, the writers
// drain and the truncate gets its turn — at the cost of being able to deadlock
// again. So the fast path asks politely, and only a fixture that has already been
// refused several times escalates to taking its place in the queue.
func (s *Store) truncateAllForTestOnce(ctx context.Context, queue bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// First, and while holding no table locks. Waiting here is bounded by the
	// holder's own lock_timeout plus its retries.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, truncateAdvisoryLockKey); err != nil {
		return err
	}
	// Bound the per-table waits so contention surfaces as a retryable 55P03
	// rather than a hang. LOCAL: reverted when this transaction ends, so a
	// pooled connection is handed back with the session default intact.
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	// Take every table lock up front with NOWAIT, which is what actually removes
	// the deadlock rather than merely retrying it. A deadlock requires waiting;
	// NOWAIT fails with 55P03 the instant a lock is held, so this transaction can
	// never sit in a wait-for cycle. The difference is who pays for contention:
	// left to plain TRUNCATE, Postgres picks a victim and it is often the ordinary
	// writer, so a test's own background goroutine dies of a deadlock it did
	// nothing to cause. Here the fixture always takes the loss and simply retries.
	//
	// Every table is locked, not just the TRUNCATE list, because CASCADE reaches
	// further than that list names (messages, turn_events, chat_input_queue …) and
	// an unlocked cascade target is exactly where a wait could still creep back in.
	// Locking the lot also means this step needs no maintenance as tables are added.
	tables, err := truncatableTableNames(ctx, tx)
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		// LOCK TABLE takes no parameters, so the table list has to be interpolated.
		// Every name came from pg_tables through format('%I') in the query above —
		// server-quoted catalog identifiers, never caller input.
		//nolint:gosec // G202: identifiers are catalog-sourced and server-quoted; LOCK TABLE cannot be parameterized.
		stmt := `LOCK TABLE ` + strings.Join(tables, ", ") + ` IN ACCESS EXCLUSIVE MODE`
		if !queue {
			stmt += ` NOWAIT`
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`TRUNCATE TABLE conversations, memories, memory_entities, users, panic_events, remote_mcp_servers, push_subscriptions, llm_providers, workspace_settings, notify_settings, turn_metrics, projects, user_connector_prefs, user_skills, shared_files RESTART IDENTITY CASCADE`); err != nil {
		return err
	}
	return tx.Commit()
}

// truncatableTableNames lists every base table in the current schema except the
// migration ledger, already quoted by format('%I') so the names can be
// concatenated into a LOCK statement. Identifiers come from the catalog, not from
// a caller, and are server-quoted before they get here.
func truncatableTableNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT format('%I.%I', schemaname, tablename)
		   FROM pg_tables
		  WHERE schemaname = current_schema()
		    AND tablename <> 'schema_migrations'
		  ORDER BY tablename`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// truncateAdvisoryLockKey namespaces the fixture's advisory lock. Arbitrary but
// fixed: every process running this helper against a shared test database must
// pick the same number for the serialization to mean anything.
const truncateAdvisoryLockKey int64 = 0x666c743174726e63 // "flt1trnc"

// isRetryableLockError reports whether err is Postgres telling us to back off
// and try the same statement again, rather than a fault in the statement:
// 40P01 deadlock_detected, 55P03 lock_not_available (our lock_timeout firing),
// 40001 serialization_failure. Matched on the typed pgconn error rather than the
// message, per the convention in users.go and sched/notes.go.
func isRetryableLockError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40P01", "55P03", "40001":
		return true
	default:
		return false
	}
}

// PanicEventRecord is the secret-safe persistence shape for one contained
// panic. Raw recovered values, stacks, tool arguments, and results are
// intentionally absent; only opaque attribution and a bounded class cross it.
type PanicEventRecord struct {
	IncidentID     string
	Location       string
	Boundary       string
	ToolName       string
	ToolCallID     string
	RunMode        string
	TaskID         string
	ConversationID string
	Class          string
}

// RecordPanicEvent appends a fully attributed recovered panic (#795). It is
// called best-effort from safe.StructuredPanicEventWriter.
func (s *Store) RecordPanicEvent(ctx context.Context, event PanicEventRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO panic_events (
			ts, incident_id, location, boundary, tool_name, tool_call_id,
			run_mode, task_id, conversation_id, message, stack
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		time.Now().Unix(), event.IncidentID, event.Location, event.Boundary,
		event.ToolName, event.ToolCallID, event.RunMode, event.TaskID,
		event.ConversationID, event.Class, "",
	)
	return err
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

// BranchConversation forks parentConvID at branchPointMessageID into a new
// conversation owned by the same user (#454): it copies the parent's messages
// with id <= branchPointMessageID into a fresh conversation (inheriting the
// parent's persona/model/lockdown), records the lineage, and returns it so the
// caller can continue the new thread independently. The branch is fully
// independent — its messages are COPIED, not shared — so deleting the parent
// never affects it. Errors if the parent is not readable by the user, or the
// branch point names no message in the parent.
//
// "Readable" is ownership OR a teammate's read of a chat its owner shared with
// the team (ADR-0057): building on a colleague's thread is the whole point of
// team-shared chats, and Branch needs no write access to the original — the
// fork belongs to the brancher from the first byte, and survives the original
// being unshared or deleted. The branch inherits the parent's project when the
// brancher is a member of it, so a fork of a project chat lands back in the
// project rather than in Temporary.
func (s *Store) BranchConversation(ctx context.Context, userEmail, parentConvID string, branchPointMessageID int64, title string) (*Conversation, error) {
	// redact narrows the message copy to what a NON-OWNER was allowed to read.
	// False for the owner's own branch, which copies the history verbatim.
	redact := false
	parent, err := s.Get(ctx, userEmail, parentConvID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		// Not the caller's own — the one other readable case is a chat a
		// teammate shared with the team.
		shared, terr := s.GetTeamVisibleConversation(ctx, userEmail, parentConvID)
		if terr != nil {
			return nil, terr
		}
		if shared == nil {
			return nil, sql.ErrNoRows
		}
		parent = &Conversation{
			ID:        shared.ID,
			UserEmail: shared.OwnerEmail,
			Title:     shared.Title,
			Persona:   shared.Persona,
			Model:     shared.Model,
			ProjectID: shared.ProjectID,
			// Lockdown rides along (see the redaction note below): the parent
			// was created --network=none under a restricted model allowlist,
			// and a fork that quietly drops that would turn "branch to build
			// on it" into a way to lift the owner's isolation.
			Lockdown: shared.Lockdown,
		}
		// The fork copies the PARENT'S messages, so it must copy no more than
		// the brancher was allowed to READ. GetTeamVisibleConversation hands a
		// teammate user/assistant text only — tool_call, tool_result and
		// reasoning entries can carry command output and API responses the
		// owner never shared (ADR-0057 §4). Without this the redaction is one
		// API call away from decorative: branch the chat you can only see the
		// prose of, then read your own copy in full.
		redact = true
	}

	// The fork is filed into the parent's project when the brancher belongs to
	// it — the membership re-check matters for the teammate path (the project
	// is what made the chat visible) and is a cheap no-op for the owner.
	projectID := ""
	if parent.ProjectID != "" {
		member, merr := s.userIsProjectMember(ctx, userEmail, parent.ProjectID)
		if merr != nil {
			return nil, merr
		}
		if member {
			projectID = parent.ProjectID
		}
	}

	// The branch point must name an actual message OF THE PARENT (#578).
	// Without this check an id above the parent's max — e.g. a stale or
	// foreign messages.id, which the global BIGSERIAL makes easy — would slip
	// through the id <= $2 copy below and silently duplicate the whole
	// conversation instead of honoring the documented "no such message → 400"
	// contract. Ids below the parent's range already fail via the empty copy.
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id = $1 AND id = $2)`,
		parentConvID, branchPointMessageID,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrBranchPointNotFound
	}

	// Load the parent's messages up to (and including) the branch point. The
	// conversation_id predicate guarantees only the PARENT's messages are
	// copied (the existence check above already rejected any id that isn't
	// the parent's). When the parent is a teammate's chat the copy is narrowed
	// to exactly what team-view showed — see the redaction note above.
	//
	// injected_context is deliberately NOT selected, so every copied entry
	// carries an empty one (ADR-0058). A turn's injected context is derived
	// per-turn state — the attachment manifest with its absolute paths, the
	// workspace inventory of the PARENT's workspace, the library listing as
	// it stood then — not something the brancher wrote or attached, and the
	// paths in it name another conversation's files. The branch recomputes
	// its own on its first turn. That is true for the owner's own fork too:
	// one rule means no copy of a message can ever carry a path into a
	// conversation that does not own it.
	copyQuery := `SELECT role, type, content FROM messages
		 WHERE conversation_id = $1 AND id <= $2`
	if redact {
		copyQuery += ` AND type = 'text' AND role IN ('user', 'assistant')`
	}
	copyQuery += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, copyQuery, parentConvID, branchPointMessageID)
	if err != nil {
		return nil, err
	}
	var entries []agent.HistoryEntry
	for rows.Next() {
		var e agent.HistoryEntry
		var content string
		if err := rows.Scan(&e.Role, &e.Type, &content); err != nil {
			_ = rows.Close()
			return nil, err
		}
		e.Content = json.RawMessage(content)
		// Rows written BEFORE migration 056 embedded the injected blocks in
		// the message text itself, so leaving injected_context unselected is
		// not enough for them: strip by marker as well. Belt-and-suspenders,
		// applied to every branch (see the copyQuery note) — a legacy row is
		// exactly the case where a path can still be riding inside the text.
		emptied, serr := stripLegacyInjectedText(&e)
		if serr != nil {
			_ = rows.Close()
			return nil, serr
		}
		if redact {
			// A user text entry carries ImageRefMeta paths pointing INTO THE
			// OWNER'S workspace, and the agent re-reads those paths verbatim
			// on the next turn (agent.loadHistoryImageParts) with no ownership
			// check. Copying them would replay the owner's uploaded images
			// into the brancher's model context — the same leak as the tool
			// rows, through a different door.
			stripped, imagesRemoved, serr := stripHistoryImages(e.Content)
			if serr != nil {
				_ = rows.Close()
				return nil, serr
			}
			e.Content = stripped
			// An image-only user message (no typed text) leaves nothing behind
			// once its images are gone. Writing the row anyway is what put
			// empty bubbles in a branched transcript — a placeholder for
			// content the copy is not allowed to carry. Emit nothing instead.
			if imagesRemoved && entryTextIsEmpty(e) {
				continue
			}
		}
		// Same rule for a legacy attachment-only message ("here" + a block,
		// or nothing but the block): once the injected suffix is off, an
		// otherwise-empty entry is residue, not content.
		if emptied {
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(entries) == 0 {
		return nil, ErrBranchPointNotFound
	}

	// Create the conversation row and copy the messages in ONE transaction
	// (#597): all-or-nothing, matching the store's atomicity convention, so a
	// crash or copy failure can never leave an empty branch shell visible in
	// the sidebar. appendHistoryTx also writes the FTS index rows + bumps
	// updated_at, so the branch is searchable like any other conversation.
	id := uuid.NewString()
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var projectArg any // NULL keeps the column's created-without-project state
	if projectID != "" {
		projectArg = projectID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversations (id, user_email, title, persona, model, pinned, lockdown, parent_conversation_id, branch_point_message_id, project_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, FALSE, $6, $7, $8, $9, $10, $11)`,
		id, userEmail, title, parent.Persona, parent.Model, parent.Lockdown, parentConvID, branchPointMessageID, projectArg, now, now,
	); err != nil {
		return nil, err
	}
	if _, err := s.appendHistoryTx(ctx, tx, id, entries); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Conversation{
		ID:                   id,
		UserEmail:            userEmail,
		Title:                title,
		Persona:              parent.Persona,
		Model:                parent.Model,
		Lockdown:             parent.Lockdown,
		ParentConversationID: parentConvID,
		BranchPointMessageID: branchPointMessageID,
		ProjectID:            projectID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}, nil
}

// stripHistoryImages drops the image references from a text history entry,
// leaving the text itself untouched, and reports whether it removed any. Used
// on the cross-user branch path, where the paths name files in another user's
// workspace. A payload that does not decode as TextContent is passed through
// unchanged — the caller's other filters already restrict this to text
// entries, and mangling an unexpected shape would be worse than copying it.
func stripHistoryImages(raw json.RawMessage) (json.RawMessage, bool, error) {
	var tc agent.TextContent
	if err := json.Unmarshal(raw, &tc); err != nil {
		return raw, false, nil //nolint:nilerr // unknown shape: pass through, see above
	}
	if len(tc.Images) == 0 {
		return raw, false, nil
	}
	tc.Images = nil
	out, err := json.Marshal(tc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// stripLegacyInjectedText removes an injected-context suffix that a
// pre-migration-056 row embedded in its own message text, in place on e. It
// reports whether the strip left the entry with no text at all (an
// attachment-only message), which the branch copy drops rather than writing an
// empty bubble.
//
// Only user text entries can carry one; everything else passes through. A
// payload that does not decode as TextContent is left alone for the same
// reason stripHistoryImages leaves it alone.
func stripLegacyInjectedText(e *agent.HistoryEntry) (emptied bool, err error) {
	if e.Role != "user" || e.Type != "text" {
		return false, nil
	}
	var tc agent.TextContent
	if uerr := json.Unmarshal(e.Content, &tc); uerr != nil {
		return false, nil //nolint:nilerr // unknown shape: pass through, see above
	}
	userText, _, stripped := agent.StripLegacyInjectedContext(tc.Text)
	if !stripped {
		return false, nil
	}
	tc.Text = userText
	out, merr := json.Marshal(tc)
	if merr != nil {
		return false, merr
	}
	e.Content = out
	return strings.TrimSpace(userText) == "" && len(tc.Images) == 0, nil
}

// entryTextIsEmpty reports whether a text entry has no visible text left. Used
// after a strip to tell "the user wrote something" from "the row only ever
// held content this copy may not carry".
func entryTextIsEmpty(e agent.HistoryEntry) bool {
	var tc agent.TextContent
	if err := json.Unmarshal(e.Content, &tc); err != nil {
		return false // unknown shape: never drop something we cannot read
	}
	return strings.TrimSpace(tc.Text) == ""
}

// userIsProjectMember reports whether email can see/use the project — the
// owner always, otherwise a shared users.team_id (Project.MemberOf's rule, as
// one query so the branch path needs no second round trip for the user row).
func (s *Store) userIsProjectMember(ctx context.Context, email, projectID string) (bool, error) {
	var member bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM projects p
			WHERE p.id = $1
			  AND (p.owner_email = $2
			       OR (p.team_id <> '' AND p.team_id = (SELECT team_id FROM users WHERE email = $2)))
		)`, projectID, normalizeEmail(email)).Scan(&member)
	if err != nil {
		return false, err
	}
	return member, nil
}

// SetOptionalMCPServers persists the user's opt-in list for this
// conversation. Callers MUST pass a normalised list (trimmed, deduped,
// lowercased, each name known to the running server). Stored as JSONB
// so we can round-trip via database/sql without pgtype plumbing.
//
// Empty list is a legal state — clears any prior opt-ins. Soft-deleted
// conversations are not mutable (deleted_at IS NULL, #596).
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
		 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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

// SetThinkingConfig sets (or clears) a conversation's extended-thinking override
// (#220). A nil cfg writes SQL NULL, restoring "inherit the global default";
// a non-nil cfg persists the explicit choice. Owner-scoped (the user_email gate),
// so one user can't toggle another's conversation. Returns "conversation not
// found" when the caller doesn't own a live conversation with that id.
func (s *Store) SetThinkingConfig(ctx context.Context, userEmail, convID string, cfg *ThinkingConfig) error {
	var arg any // nil → SQL NULL → inherit global default
	if cfg != nil {
		payload, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal thinking config: %w", err)
		}
		arg = payload
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET thinking_config = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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

// scanThinkingConfig decodes the nullable thinking_config JSONB. NULL, empty, or
// a `null` literal yield nil (inherit the global default); a malformed payload
// also yields nil rather than erroring, so a bad row never blocks the rest of the
// conversation record (mirrors scanOptionalMCPServers' tolerance).
func scanThinkingConfig(raw []byte) *ThinkingConfig {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tc ThinkingConfig
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	return &tc
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

// scanMCPAccounts decodes the conversations.mcp_accounts JSONB object; a
// missing/empty/undecodable value is "no overrides" (nil).
func scanMCPAccounts(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// SetConversationMCPAccounts replaces a conversation's per-connector seat
// overrides (#988): server name → account label, already validated by the
// HTTP layer against the live seat catalog. nil/empty clears every override
// (back to the user's defaults). Owner-scoped; soft-deleted conversations
// are not mutable (#596).
func (s *Store) SetConversationMCPAccounts(ctx context.Context, userEmail, convID string, accounts map[string]string) error {
	if accounts == nil {
		accounts = map[string]string{}
	}
	payload, err := json.Marshal(accounts)
	if err != nil {
		return fmt.Errorf("marshal mcp accounts: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET mcp_accounts = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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

// SetModel updates the per-chat OpenRouter slug. Empty model clears the
// stored value; the frontend will supply its DEFAULT_MODEL on the next turn.
// deleted_at IS NULL matches SetShareToken: a soft-deleted conversation is
// not mutable (#596) — the guard is applied uniformly across the setters.
func (s *Store) SetModel(ctx context.Context, userEmail, convID, model string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET model = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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
// caller treats as a benign skip. A soft-deleted conversation is likewise
// skipped (deleted_at IS NULL, #596).
func (s *Store) UpdateTitle(ctx context.Context, userEmail, convID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND title_locked = FALSE AND deleted_at IS NULL`,
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
// always wins and pins the name against the auto-titler thereafter. A
// soft-deleted conversation is not mutable (deleted_at IS NULL, #596).
func (s *Store) RenameTitle(ctx context.Context, userEmail, convID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = $1, title_locked = TRUE, updated_at = $2 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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

// SetPinned toggles the pin state for a conversation. A soft-deleted
// conversation is not mutable (deleted_at IS NULL, #596).
func (s *Store) SetPinned(ctx context.Context, userEmail, convID string, pinned bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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
// A soft-deleted conversation is not mutable (deleted_at IS NULL, #596).
func (s *Store) SetArchived(ctx context.Context, userEmail, convID string, archived bool) error {
	now := time.Now().Unix()
	var archivedAt any // NULL when unarchiving
	pinned := false    // archiving always unpins; unarchiving leaves it unpinned
	if archived {
		archivedAt = now
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET archived_at = $1, pinned = $2, updated_at = $3 WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL`,
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
// only touch their own conversations. A soft-deleted conversation is not
// mutable (deleted_at IS NULL, #596).
func (s *Store) SetApprovalTimeout(ctx context.Context, userEmail, convID string, seconds *int) error {
	var arg any // NULL when clearing
	if seconds != nil {
		arg = *seconds
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET approval_timeout_seconds = $1, updated_at = $2 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
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
			// Drop the persisted messages.id from the PUBLIC snapshot. LoadHistory
			// populates it for the owner's branching flow (#454), but the #226 share
			// contract deliberately omits internal identifiers — a global BIGSERIAL id
			// would leak cross-conversation row ordering/volume to an anonymous viewer.
			m.ID = 0
			out.Messages = append(out.Messages, m)
		}
	}
	return &out, nil
}

// AutoArchiveOlderThan archives unpinned, not-already-archived conversations
// whose updated_at is older than d (#282). Returns the count archived. A zero or
// negative duration is a no-op (the feature is disabled). This is a softer
// alternative to the TTL hard-delete in SweepExpired — conversations are filed
// away rather than destroyed. Project conversations (#509) are exempt like
// they are from the sweep: auto-archiving one would silently pull it out of
// its project's rail tree.
func (s *Store) AutoArchiveOlderThan(ctx context.Context, d time.Duration) (int, error) {
	if d <= 0 {
		return 0, nil
	}
	now := time.Now().Unix()
	cutoff := time.Now().Add(-d).Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET archived_at = $1, updated_at = $2
		 WHERE pinned = FALSE AND archived_at IS NULL AND project_id IS NULL AND updated_at < $3`,
		now, now, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("auto-archive: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Delete removes a conversation and (via FK cascade) its content rows. Usage
// metrics deliberately survive hard deletion: they are accounting records and
// retain no transcript content (migration 038). When
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
//
// Project conversations are exempt too (#509), keying the same way the rail's
// Temporary section and the retention sweep already do: filing a chat into a
// project takes it OUT of the list this action clears, so sweeping it anyway
// deleted a chat the user could not see in the section they were emptying —
// and made "chats in a project don't expire" false at the one moment it
// mattered most.
func (s *Store) DeleteAllUnpinned(ctx context.Context, userEmail string) (int, error) {
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE user_email = $2 AND pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND project_id IS NULL`,
			time.Now().Unix(), userEmail,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE user_email = $1 AND pinned = FALSE AND archived_at IS NULL AND project_id IS NULL`,
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
		ids, userEmail,
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
			time.Now().Unix(), ids, userEmail,
		)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id = ANY($1) AND user_email = $2`,
		ids, userEmail,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteAllMatching removes (or, in soft-delete mode, tombstones) every unpinned
// conversation for the user carrying the optional label. An empty label is "no
// label filter". Returns the count affected.
//
// The filter is bound as a parameter with an `$n = ”` short-circuit so no SQL is
// concatenated from the input (defense against injection-by-clause).
//
// Project conversations are exempt, for the same reason DeleteAllUnpinned
// exempts them: filing is a "keep" state, and a project chat is not in the
// list this action clears. Deleting a specific project chat stays possible —
// through the targeted DeleteByIDs, which the user reaches by selecting it.
func (s *Store) DeleteAllMatching(ctx context.Context, userEmail, label string) (int, error) {
	now := time.Now().Unix()
	if s.softDelete {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET deleted_at = NOW(), updated_at = $1
			 WHERE user_email = $2 AND pinned = FALSE AND deleted_at IS NULL AND project_id IS NULL
			   AND ($3 = '' OR $3 = ANY(labels))`,
			now, userEmail, label,
		)
		if err != nil {
			return 0, err
		}
		c, _ := res.RowsAffected()
		return int(c), nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations
		 WHERE user_email = $1 AND pinned = FALSE AND deleted_at IS NULL AND project_id IS NULL
		   AND ($2 = '' OR $2 = ANY(labels))`,
		userEmail, label,
	)
	if err != nil {
		return 0, err
	}
	c, _ := res.RowsAffected()
	return int(c), nil
}

// BulkPatch applies the supplied mutations to the conversations identified by
// ids in a single transaction. A nil pointer (pinned / labels) means "leave that
// field untouched"; a non-nil pointer — including an empty labels slice —
// overwrites the stored value. Returns ErrForeignConversation (mapped to 403) if
// any supplied ID is foreign or unknown; the transaction rolls back so nothing
// is mutated. The caller caps len(ids) (HTTP layer enforces 100).
func (s *Store) BulkPatch(ctx context.Context, userEmail string, ids []string, pinned *bool, labels []string) (int, error) {
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
		ids, userEmail,
	).Scan(&owned); err != nil {
		return 0, err
	}
	if owned != len(ids) {
		return 0, ErrForeignConversation
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE conversations
         SET pinned     = COALESCE($3, pinned),
             labels     = COALESCE($4, labels),
             updated_at = $5
         WHERE id = ANY($1) AND user_email = $2 AND deleted_at IS NULL`,
		ids, userEmail,
		pinned, labels,
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

// ListFilter constrains ListFiltered (#258). The zero value lists all active
// conversations (the default sidebar view). ArchivedOnly selects the archived
// section instead (#282). Labels has AND semantics — every listed label must be
// present (Postgres array containment).
type ListFilter struct {
	ArchivedOnly bool
	Labels       []string
}

// ListFiltered returns the user's conversations matching f, pinned first, newest
// first. See ListFilter for the filter semantics (#258). When ArchivedOnly is
// false it returns only active (archived_at IS NULL) conversations — the default
// sidebar view; when true it returns only the archived ones (#282), so the
// frontend can render them in a separate, collapsed section.
func (s *Store) ListFiltered(ctx context.Context, userEmail string, f ListFilter) ([]Conversation, error) {
	// A single CONSTANT query with a sentinel-guarded optional filter (mirroring
	// DeleteAllMatching) — no string concatenation, so every value is bound and
	// the query plan is stable. $2 picks the active/archived partition; $3 (NULL =
	// no label filter) does AND-containment.
	var labelsArg any
	if len(f.Labels) > 0 {
		labelsArg = f.Labels
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationColumns+`
		 FROM conversations
		 WHERE user_email = $1 AND deleted_at IS NULL
		   AND (CASE WHEN $2 THEN archived_at IS NOT NULL ELSE archived_at IS NULL END)
		   AND ($3::text[] IS NULL OR labels @> $3::text[])
		 ORDER BY pinned DESC, updated_at DESC, id DESC`,
		userEmail, f.ArchivedOnly, labelsArg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationRows(rows)
}

// conversationColumns is the SELECT list (in scan order) every conversation query
// shares, so Get, ListFiltered and ListTeamConversations stay in lockstep with
// scanConversation. (Bare column names, so a `c.`-aliased query can use it too.)
const conversationColumns = `id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at, archived_at, title_locked, optional_mcp_servers_enabled, labels, approval_timeout_seconds, COALESCE(share_token, ''), COALESCE(parent_conversation_id, ''), COALESCE(branch_point_message_id, 0), thinking_config, COALESCE(project_id, ''), mcp_accounts, team_visible`

// teamConversationColumns is conversationColumns with everything a teammate
// has no business reading blanked out. Column count and scan order stay
// identical so scanConversation serves both.
//
// What a team listing is FOR is "which chats did my colleagues share, and what
// are they called" — the row's identity and its title. Everything else on a
// conversation is the owner's working state, and several fields are actively
// disclosive:
//
//   - share_token — the public capability URL (#1112). Harvesting it from a
//     team listing would turn "shared with my team" into "shared with anyone".
//   - labels — user-chosen, and people name them after what the work is
//     ("acme-dd-confidential", "layoffs-2026"). A label is not part of what
//     opting a chat into team visibility offers.
//   - mcp_accounts — which connector SEAT the owner used, keyed by a label
//     that is very often an address.
//   - optional_mcp_servers_enabled — the owner's per-chat connector loadout.
//   - parent_conversation_id — reveals that a shared chat was branched from a
//     private one, and hands over that private chat's id.
const teamConversationColumns = `id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at, archived_at, title_locked, FALSE AS optional_mcp_servers_enabled, NULL::text[] AS labels, approval_timeout_seconds, '' AS share_token, '' AS parent_conversation_id, COALESCE(branch_point_message_id, 0), thinking_config, COALESCE(project_id, ''), NULL::jsonb AS mcp_accounts, team_visible`

// pgTypeMap is the pgx type map used to decode Postgres array literals reaching
// this package over database/sql. It is read-only after init and safe for
// concurrent use.
var pgTypeMap = pgtype.NewMap()

// textArray is a sql.Scanner for a Postgres text[] column.
//
// Binding an array needs no wrapper — the pgx stdlib driver implements
// driver.NamedValueChecker, so a plain Go slice ([]string) is handed to pgx and
// encoded as an array. Scanning is the asymmetric half: database/sql receives
// the column as pgx's text-format array LITERAL (e.g. `{a,"b,c"}`) and cannot
// convert that string into a *[]string on its own. Rather than hand-roll a
// literal parser — labels are user-supplied and may contain commas, quotes,
// backslashes, braces, or the bare word NULL — this delegates to pgx's own
// array codec.
//
// A SQL NULL decodes to a nil slice, matching the column's absent-value sense.
type textArray struct{ vals *[]string }

func (a textArray) Scan(src any) error {
	if src == nil {
		*a.vals = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return fmt.Errorf("textArray: cannot scan %T into []string", src)
	}
	if err := pgTypeMap.Scan(pgtype.TextArrayOID, pgtype.TextFormatCode, raw, a.vals); err != nil {
		return fmt.Errorf("textArray: decode text[]: %w", err)
	}
	return nil
}

// scanConversation scans one row produced with conversationColumns. It takes the
// package's rowScanner (the one method *sql.Row and *sql.Rows share) so the
// single-row Get and the row-by-row list scans share one scan order.
func scanConversation(sc rowScanner) (Conversation, error) {
	var c Conversation
	var optionalRaw []byte
	var approvalTimeout sql.NullInt64
	var thinkingRaw []byte
	var accountsRaw []byte
	if err := sc.Scan(&c.ID, &c.UserEmail, &c.Title, &c.Persona, &c.Model, &c.Pinned, &c.Lockdown, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.TitleLocked, &optionalRaw, textArray{&c.Labels}, &approvalTimeout, &c.ShareToken, &c.ParentConversationID, &c.BranchPointMessageID, &thinkingRaw, &c.ProjectID, &accountsRaw, &c.TeamVisible); err != nil {
		return Conversation{}, err
	}
	c.OptionalMCPServersEnabled = scanOptionalMCPServers(optionalRaw)
	c.MCPAccounts = scanMCPAccounts(accountsRaw)
	c.ApprovalTimeoutSeconds = nullableSeconds(approvalTimeout)
	c.ThinkingConfig = scanThinkingConfig(thinkingRaw)
	return c, nil
}

// scanConversationRows scans a rows set produced with conversationColumns.
func scanConversationRows(rows *sql.Rows) ([]Conversation, error) {
	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrNoTeam is returned by ListTeamConversations when the caller has no team_id —
// there is no team to scope to. The HTTP layer maps it to 400.
var ErrNoTeam = errors.New("caller has no team")

// ListTeamConversations returns the active conversations SHARED WITH the
// caller's team (team_visible = TRUE), read-only (#237). Team membership alone
// never exposes a conversation — only the owner's explicit share-with-team
// does — so this preserves the per-user privacy default while enabling
// "manager sees the team's shared threads". Returns ErrNoTeam when the caller
// has no team_id. Gated by the per-conversation opt-in AND the audience the
// owner named when they opted in (see ADR-0013, amended by ADR-0057).
//
// The audience comes from team_shared_with, NOT from the owner's current
// users.team_id. Matching on the owner's team meant an admin moving that owner
// to another team silently handed every chat they had shared to the new team —
// a group the owner never chose. A stamped audience cannot drift.
func (s *Store) ListTeamConversations(ctx context.Context, callerEmail string) ([]Conversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	caller, err := s.GetUser(ctx, callerEmail)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(caller.TeamID) == "" {
		return nil, ErrNoTeam
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+teamConversationColumns+`
		FROM conversations c
		WHERE c.deleted_at IS NULL
		  AND c.archived_at IS NULL
		  AND c.team_visible = TRUE
		  AND c.team_shared_with = $1
		ORDER BY c.pinned DESC, c.updated_at DESC, c.id DESC`,
		caller.TeamID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanConversationRows(rows)
	if err != nil {
		return nil, err
	}
	// Belt-and-suspenders: never serialize a teammate's share token even
	// if the SELECT list drifts back to conversationColumns.
	for i := range list {
		list[i].ShareToken = ""
	}
	return list, nil
}

// ErrNoTeamToShareWith and ErrNoTeamShareHome are the two ways a request to
// team-share a chat is refused for want of an audience or a place to appear.
// They are distinct so the UI can say which situation the user is in.
var (
	// ErrNoTeamToShareWith: the owner belongs to no team, so there is nobody
	// to name as the audience.
	ErrNoTeamToShareWith = errors.New("you are not in a team yet, so there is nobody to share this chat with")
	// ErrNoTeamShareHome: the chat is not in a project shared with the owner's
	// team, so a teammate would have no surface listing it.
	ErrNoTeamShareHome = errors.New("a chat can only be shared with your team from inside a project that is shared with that team")
)

// SetConversationTeamVisible flips a conversation's team_visible flag (#237)
// and reports the state that was actually stored. Only the OWNER may change it
// (the WHERE user_email gate), so one teammate can't expose another's
// conversation. Returns ErrConversationNotFound when the caller doesn't own a
// conversation with that id.
//
// Opting in STAMPS THE AUDIENCE (team_shared_with = the owner's team at that
// moment) rather than leaving readers to infer it from the owner's current team
// (ADR-0057, migration 054). The inference was wrong the moment the owner moved
// teams: an admin reassigning the owner silently re-pointed every chat they had
// shared at the new team. Opting out clears the stamp, so "not shared" has one
// representation.
//
// Opting in ALSO REQUIRES A HOME — the chat must sit in a project shared with
// that same team — and refuses with ErrNoTeamToShareWith / ErrNoTeamShareHome
// otherwise. ADR-0057 first left this narrowing to the UI. That was wrong: the
// pairing's every revocation path (leaving the team, unsharing or deleting the
// project) is keyed on the PROJECT, so a share with no project matched none of
// them and no surface could revoke it — a share that outlives its owner's
// membership with no way to take it back. Enforcing it here is what makes "a
// team-shared chat always has a home" true rather than hoped for.
//
// Opting OUT is never refused. Revocation must work from whatever state a row
// is in, including one a pre-054 client created.
func (s *Store) SetConversationTeamVisible(ctx context.Context, ownerEmail, convID string, visible bool) (bool, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	now := time.Now().Unix()
	if !visible {
		res, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET team_visible = FALSE, team_shared_with = NULL, updated_at = $1
			 WHERE id = $2 AND user_email = $3 AND deleted_at IS NULL`,
			now, convID, ownerEmail)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, errors.New("conversation not found")
		}
		return false, nil
	}

	// Ownership first, so the three refusals keep their precedence: a caller
	// who owns no such chat learns nothing about teams or projects (404), and
	// only an owner sees which of the two share preconditions they are missing.
	var homeTeam sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT p.team_id
		FROM conversations c
		LEFT JOIN projects p ON p.id = c.project_id
		WHERE c.id = $1 AND c.user_email = $2 AND c.deleted_at IS NULL`,
		convID, ownerEmail).Scan(&homeTeam); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, errors.New("conversation not found")
		}
		return false, err
	}
	var ownerTeam sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT team_id FROM users WHERE email = $1`, ownerEmail).Scan(&ownerTeam); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	team := strings.TrimSpace(ownerTeam.String)
	if team == "" {
		return false, ErrNoTeamToShareWith
	}
	if strings.TrimSpace(homeTeam.String) != team {
		return false, ErrNoTeamShareHome
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations c SET
			team_visible = TRUE, team_shared_with = $1, updated_at = $2
		 WHERE c.id = $3 AND c.user_email = $4 AND c.deleted_at IS NULL
		   AND EXISTS (SELECT 1 FROM projects p WHERE p.id = c.project_id AND p.team_id = $1)`,
		team, now, convID, ownerEmail)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost a race with a write that moved the chat or unshared the project.
		return false, ErrNoTeamShareHome
	}
	return true, nil
}

// List returns the user's active (or, when archivedOnly, archived) conversations,
// pinned first, newest first. Thin wrapper over ListFiltered preserved for the
// many callers that don't filter by label.
func (s *Store) List(ctx context.Context, userEmail string, archivedOnly bool) ([]Conversation, error) {
	return s.ListFiltered(ctx, userEmail, ListFilter{ArchivedOnly: archivedOnly})
}

// Get fetches a single conversation (without messages).
func (s *Store) Get(ctx context.Context, userEmail, convID string) (*Conversation, error) {
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationColumns+`
		 FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL`,
		convID, userEmail,
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// LoadHistory returns every stored message event for a conversation in
// insertion order.
//
// injected_context (migration 056) rides along on HistoryEntry so replay hands
// the model the same bytes it saw originally. That field is `json:"-"`, so it
// reaches a client only through a response shape that names it explicitly —
// the public share snapshot and the team-shared read view, both of which
// project straight from this call, must not publish another user's attachment
// paths. See agent/injected.go and ADR-0058.
func (s *Store) LoadHistory(ctx context.Context, convID string) ([]agent.HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, type, content, injected_context
		   FROM messages WHERE conversation_id = $1 ORDER BY id ASC`,
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
		if err := rows.Scan(&e.ID, &e.Role, &e.Type, &content, &e.InjectedContext); err != nil {
			return nil, err
		}
		e.Content = json.RawMessage(content)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendHistory writes every entry in turn order and bumps the conversation
// updated_at. Done inside a single transaction so partial writes don't leave
// torn state if the process dies mid-turn. Returns the inserted message ids in
// entry order, so the turn stream can tell the client which persisted rows its
// in-flight messages became (the Branch button needs a dbId, #454).
func (s *Store) AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := s.appendHistoryTx(ctx, tx, convID, entries)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// appendHistoryTx is AppendHistory's body against a caller-owned transaction,
// so a multi-statement op (BranchConversation's row + copy, #597) can make the
// message write part of its own atomic unit. The caller commits or rolls back.
func (s *Store) appendHistoryTx(ctx context.Context, tx *sql.Tx, convID string, entries []agent.HistoryEntry) ([]int64, error) {
	now := time.Now().Unix()

	var b strings.Builder
	b.WriteString(`INSERT INTO messages (conversation_id, role, type, content, created_at, injected_context) VALUES `)
	args := make([]any, 0, len(entries)*6)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*6 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d)", base, base+1, base+2, base+3, base+4, base+5)
		// InjectedContext is empty for every entry this path writes today
		// except a deliberate carry-over; the branch copy CLEARS it on
		// purpose (ADR-0058), so writing the field rather than defaulting the
		// column keeps that intent visible instead of implicit.
		args = append(args, convID, e.Role, e.Type, string(e.Content), now, e.InjectedContext)
	}
	// RETURNING id (in VALUES order) so we can link the extracted FTS plaintext
	// rows back to their messages. Postgres preserves multi-row INSERT order.
	b.WriteString(" RETURNING id")
	ids := make([]int64, 0, len(entries))
	rows, err := tx.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	// Close before the next statement on this tx (one active result set at a time).
	_ = rows.Close()
	if len(ids) != len(entries) {
		return nil, fmt.Errorf("AppendHistory: inserted %d messages but got %d ids", len(entries), len(ids))
	}

	// Full-text search index maintenance (#308): extract searchable plaintext from
	// the just-inserted messages into message_search_content, in the same tx.
	if s.searchEnabled {
		if err := insertSearchContent(ctx, tx, convID, now, entries, ids); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, now, convID); err != nil {
		return nil, err
	}
	return ids, nil
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
	// MCPServer / MCPAccount record the credential seat that was active when
	// the call was staged (#167 residual 2). A card outlives its turn scope,
	// so execution reopens a scope from this pair instead of silently running
	// on the broker's default bundle seat. MCPServer is empty for native tools
	// and for rows staged before the columns existed; MCPAccount empty means
	// the default seat.
	MCPServer  string
	MCPAccount string
}

// ListPendingApprovals returns every pending approval for a conversation,
// oldest first. Used on page reload to re-render approval cards that were
// staged but never resolved in the previous browser session.
func (s *Store) ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
		        COALESCE(mcp_server, ''), COALESCE(mcp_account, '')
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
			&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
			&a.MCPServer, &a.MCPAccount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MaxResolvedApprovalsPerConversation caps how many resolved approvals the
// conversation GET re-hydrates as transcript cards. Approvals are one row per
// human-gated action, so a conversation rarely accumulates more than a
// handful; the cap only bounds a pathological chat's payload. Newest-first is
// applied in SQL so the cap keeps the RECENT cards, then callers re-sort.
const MaxResolvedApprovalsPerConversation = 100

// ListResolvedApprovals returns the most recent non-pending approvals for a
// conversation, oldest first (capped at MaxResolvedApprovalsPerConversation).
// Used on page reload so resolved approval cards — the "Email sent ✓" outcome,
// a notify-mode "ran without asking" record and its undo hint, a timed-out
// card — survive a reload instead of the transcript silently changing shape
// the first time the user leaves and comes back (#1153's record contract).
func (s *Store) ListResolvedApprovals(ctx context.Context, userEmail, convID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
		        COALESCE(mcp_server, ''), COALESCE(mcp_account, '')
		 FROM approvals
		 WHERE conversation_id = $1 AND user_email = $2 AND status <> 'pending'
		 ORDER BY created_at DESC
		 LIMIT $3`,
		convID, userEmail, MaxResolvedApprovalsPerConversation,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
			&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
			&a.MCPServer, &a.MCPAccount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Oldest first for display, matching ListPendingApprovals.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// CreateApproval stages a pending approval and returns the row.
// toolCallID is the agent-assigned id of the tool_call event being
// staged; empty is allowed (older code paths) but populating it lets
// the post-approval resolver write its tool_result back under the same
// id the UI chip is keyed on.
// expiresAt is the unix-seconds default-deny deadline for the staged approval
// (#225); pass 0 for "no expiry" (the column is stored NULL and the server-side
// expiry sweep skips the row).
// seat records the credential {server, account} the staging turn was running on
// (#167 residual 2). A zero ApprovalSeat is a native tool or an unknown seat and
// stores NULL, which execution reads back as "the default bundle seat".
func (s *Store) CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string, expiresAt int64, seat ApprovalSeat) (*Approval, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	var expiresArg any // NULL when there is no timeout
	if expiresAt > 0 {
		expiresArg = expiresAt
	}
	// The seat is stored only when a server authored the call. Writing NULL for
	// a native tool keeps "no seat recorded" distinct from "the default seat was
	// deliberately chosen", which is what a recorded server + empty account means.
	var serverArg, accountArg any
	if seat.Server != "" {
		serverArg, accountArg = seat.Server, seat.Account
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (id, conversation_id, user_email, tool_name, tool_call_id, args_json, status, created_at, expires_at, mcp_server, mcp_account)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9, $10)`,
		id, convID, userEmail, toolName, toolCallID, argsJSON, now, expiresArg, serverArg, accountArg,
	)
	if err != nil {
		return nil, err
	}
	return &Approval{
		ID: id, ConversationID: convID, UserEmail: userEmail,
		ToolName: toolName, ToolCallID: toolCallID, ArgsJSON: argsJSON, Status: approvalStatusPending,
		CreatedAt: now, ExpiresAt: expiresAt,
		MCPServer: seat.Server, MCPAccount: seat.Account,
	}, nil
}

// ApprovalSeat is the public credential selection a staged MCP call ran under.
// Both fields are configuration identifiers; no credential value is stored.
type ApprovalSeat struct {
	// Server is the bundle server name that exports the staged tool. Empty for
	// native tools (bash, preview_email).
	Server string
	// Account is the named seat, or empty for the default bundle seat.
	Account string
}

// GetApproval looks up a pending approval, scoped by user_email.
func (s *Store) GetApproval(ctx context.Context, userEmail, approvalID string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, conversation_id, user_email, tool_name, args_json, status,
		        COALESCE(result_text, ''), created_at, COALESCE(resolved_at, 0),
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
		        COALESCE(mcp_server, ''), COALESCE(mcp_account, '')
		 FROM approvals WHERE id = $1 AND user_email = $2`,
		approvalID, userEmail,
	)
	var a Approval
	if err := row.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
		&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
		&a.MCPServer, &a.MCPAccount); err != nil {
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
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
		        COALESCE(mcp_server, ''), COALESCE(mcp_account, '')
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
			&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
			&a.MCPServer, &a.MCPAccount); err != nil {
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
//
// Expired rows are not claimable: a still-pending approval past its
// expires_at deadline is default-deny (#225 / #1109), regardless of
// whether SweepExpiredApprovals has run yet. Rows with NULL/0
// expires_at never expire. The expiry sweep uses ClaimExpiredApproval
// so it can still flip those rows for notification/audit.
func (s *Store) ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error) {
	if !validApprovalResolution(newStatus) {
		return false, fmt.Errorf("invalid approval status %q", newStatus)
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'
		   AND (expires_at IS NULL OR expires_at = 0 OR expires_at > $6)`,
		newStatus, resultText, now, approvalID, userEmail, now,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimExpiredApproval atomically rejects (or otherwise resolves) a
// pending approval whose expires_at deadline has already passed. Used
// only by the expiry sweep (#225) so notification/audit still happens
// after ClaimApproval starts refusing expired rows (#1109).
func (s *Store) ClaimExpiredApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error) {
	if !validApprovalResolution(newStatus) {
		return false, fmt.Errorf("invalid approval status %q", newStatus)
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status = $1, result_text = $2, resolved_at = $3
		 WHERE id = $4 AND user_email = $5 AND status = 'pending'
		   AND expires_at IS NOT NULL AND expires_at > 0 AND expires_at <= $6`,
		newStatus, resultText, now, approvalID, userEmail, now,
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
		        COALESCE(tool_call_id, ''), COALESCE(expires_at, 0),
		        COALESCE(mcp_server, ''), COALESCE(mcp_account, '')
		 FROM approvals
		 WHERE conversation_id = $1 AND tool_name = $2
		 ORDER BY created_at DESC
		 LIMIT 1`,
		convID, toolName,
	)
	var a Approval
	if err := row.Scan(&a.ID, &a.ConversationID, &a.UserEmail, &a.ToolName,
		&a.ArgsJSON, &a.Status, &a.ResultText, &a.CreatedAt, &a.ResolvedAt, &a.ToolCallID, &a.ExpiresAt,
		&a.MCPServer, &a.MCPAccount); err != nil {
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
// whole thing returns in milliseconds. deleted_at IS NULL matches every
// other conversation read (#579): under soft-delete a tombstoned row must
// not inflate the counts or masquerade as recent activity (Delete bumps
// updated_at, so an unfiltered MAX would surface the deletion itself).
func (s *Store) AdminStats(ctx context.Context) ([]AdminRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT
		    c.user_email                                 AS email,
		    COUNT(c.id)                                  AS conv_count,
		    SUM(CASE WHEN c.pinned THEN 1 ELSE 0 END)    AS pinned_count,
		    MAX(c.updated_at)                            AS last_activity
		 FROM conversations c
		 WHERE c.deleted_at IS NULL
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
	// Project conversations (#509) are exempt the same way: filing a chat
	// into a project is a deliberate "keep" act, and the rail presents
	// projects as durable workspaces — silently reaping their chats would
	// betray that.

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
			 WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND project_id IS NULL AND updated_at < $2`,
			time.Now().Unix(), cutoff,
		)
		if err != nil {
			return expired, 0, fmt.Errorf("ttl sweep: %w", err)
		}
		n, _ := res.RowsAffected()
		expired += int(n)
	} else {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM conversations WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND project_id IS NULL AND updated_at < $1`,
			cutoff,
		)
		if err != nil {
			return expired, 0, fmt.Errorf("ttl sweep: %w", err)
		}
		n, _ := res.RowsAffected()
		expired += int(n)
	}

	// 2. Per-user cap, in ONE statement. The previous shape scanned for
	//    overflowing users and then issued a DELETE per user — a round trip
	//    per user on a path that runs after every successful turn. ROW_NUMBER()
	//    partitioned by user_email ranks each user's rows independently, so
	//    `rn > cap` selects exactly what the per-user `ORDER BY updated_at DESC,
	//    id DESC OFFSET cap` selected, for every user at once. Users at or under
	//    the cap contribute no rows, so the HAVING pre-scan is redundant.
	if unpinnedCap <= 0 {
		return expired, 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM conversations WHERE id IN (
		    SELECT id FROM (
		        SELECT id, ROW_NUMBER() OVER (
		            PARTITION BY user_email ORDER BY updated_at DESC, id DESC
		        ) AS rn
		        FROM conversations
		        WHERE pinned = FALSE AND archived_at IS NULL AND deleted_at IS NULL AND share_token IS NULL AND project_id IS NULL
		    ) ranked
		    WHERE rn > $1
		 )`,
		unpinnedCap,
	)
	if err != nil {
		return expired, evicted, fmt.Errorf("cap evict: %w", err)
	}
	n, _ := res.RowsAffected()
	evicted += int(n)
	return expired, evicted, nil
}
