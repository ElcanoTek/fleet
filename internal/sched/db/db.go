// Package db provides the PostgreSQL database layer for the fleet orchestrator
// (sched). Ported from moc's internal/db and converged from lib/pq onto
// jackc/pgx/v5 (registered through the stdlib database/sql adapter). The one
// schema change vs moc: per-task target_node_* routing is replaced by an
// mcp_selection JSONB column (plan §6.2).
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Database is the PostgreSQL database wrapper for the orchestrator.
type Database struct {
	conn *sql.DB

	// archiveKey is the optional 32-byte AES-256-GCM key for log archival (#272).
	// Held host-side and NEVER logged. nil = archives are gzip-only (no
	// encryption). Set once via SetLogArchiveKey before the archival sweep runs.
	archiveKey []byte
}

// SetLogArchiveKey configures the host-side AES-256-GCM key used to encrypt
// archived log payloads (#272). A nil/empty key disables encryption (archives
// are gzip-only). The key is held in memory only and never logged or persisted.
// It must be exactly 32 bytes; a wrong length surfaces only when the archival
// sweep or a read of an encrypted archive runs.
func (db *Database) SetLogArchiveKey(key []byte) { db.archiveKey = key }

// New creates a new Database instance.
func New() *Database {
	return &Database{}
}

// Init initializes the database connection and schema. Accepts a connection
// string or reads from DATABASE_URL. A legacy file-path argument (leading '.'
// or '/', or empty) is ignored in favor of DATABASE_URL / DB_* env vars.
// PoolConfig tunes the sched DB connection pool (#276). Local to this package
// (the cmd layer maps env-derived config into it) to keep it decoupled from
// internal/config. DefaultPoolConfig reproduces the historical hard-coded values.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultPoolConfig is the behavior-preserving baseline: 25 open / 5 idle, 5m
// lifetime, 10s connect ping (sched historically pinged at 10s).
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// Stats returns the connection-pool snapshot for metrics (#276).
func (db *Database) Stats() sql.DBStats { return db.conn.Stats() }

// Ping verifies the sched DB is reachable (readiness probe, #215).
func (db *Database) Ping(ctx context.Context) error { return db.conn.PingContext(ctx) }

func (db *Database) Init(connStr string, pool PoolConfig) error {
	if connStr == "" || connStr[0] == '.' || connStr[0] == '/' {
		connStr = os.Getenv("DATABASE_URL")
		if connStr == "" {
			host := getEnvOrDefault("DB_HOST", "localhost")
			port := getEnvOrDefault("DB_PORT", "5432")
			user := getEnvOrDefault("DB_USER", "fleet")
			password := getEnvOrDefault("DB_PASSWORD", "")
			dbname := getEnvOrDefault("DB_NAME", "sched")
			sslmode := getEnvOrDefault("DB_SSLMODE", "disable")
			connStr = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
				host, port, user, password, dbname, sslmode)
		}
	}

	conn, err := sql.Open("pgx", connStr)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	db.conn = conn

	db.conn.SetMaxOpenConns(pool.MaxOpenConns)
	db.conn.SetMaxIdleConns(pool.MaxIdleConns)
	db.conn.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	db.conn.SetConnMaxLifetime(pool.ConnMaxLifetime)

	connectTimeout := pool.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := db.conn.PingContext(pingCtx); err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := RunMigrations(db.conn); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	return nil
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// Close closes the database connection.
func (db *Database) Close() error {
	if db.conn != nil {
		return db.conn.Close()
	}
	return nil
}

// Conn returns the underlying database connection for transaction support.
func (db *Database) Conn() *sql.DB {
	return db.conn
}

func marshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("Warning: failed to marshal JSON: %v (value type: %T)", err, v)
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(s string) []string {
	if s == "" {
		return []string{}
	}
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Printf("Warning: failed to unmarshal string slice: %v (input: %.100s)", err, s)
		return []string{}
	}
	if result == nil {
		return []string{}
	}
	return result
}

func unmarshalMCPSelection(s string) models.MCPSelection {
	if s == "" {
		return models.MCPSelection{}
	}
	var result models.MCPSelection
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Printf("Warning: failed to unmarshal mcp_selection: %v (input: %.100s)", err, s)
		return models.MCPSelection{}
	}
	if result == nil {
		return models.MCPSelection{}
	}
	return result
}

// uuidStrings converts a slice of UUIDs to their string forms for array params.
func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// User operations

// AddUser adds or updates a user.
func (db *Database) AddUser(ctx context.Context, user *models.User) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO users (
			id, username, password_hash, role, created_at, last_login, session_token, token_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			username = EXCLUDED.username,
			password_hash = EXCLUDED.password_hash,
			role = EXCLUDED.role,
			last_login = EXCLUDED.last_login,
			session_token = EXCLUDED.session_token,
			token_expires_at = EXCLUDED.token_expires_at`,
		user.ID,
		user.Username,
		user.PasswordHash,
		user.Role,
		user.CreatedAt,
		user.LastLogin,
		user.SessionToken,
		user.TokenExpiresAt,
	)
	return err
}

// UpdateUserRole changes an existing user's role.
func (db *Database) UpdateUserRole(ctx context.Context, userID uuid.UUID, role string) error {
	res, err := db.conn.ExecContext(ctx,
		"UPDATE users SET role = $1 WHERE id = $2", role, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RenameUser changes an existing user's username.
func (db *Database) RenameUser(ctx context.Context, userID uuid.UUID, newUsername string) error {
	res, err := db.conn.ExecContext(ctx,
		"UPDATE users SET username = $1 WHERE id = $2", newUsername, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteUser removes a user by ID.
func (db *Database) DeleteUser(ctx context.Context, userID uuid.UUID) error {
	res, err := db.conn.ExecContext(ctx,
		"DELETE FROM users WHERE id = $1", userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetUser gets a user by ID.
func (db *Database) GetUser(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, created_at, last_login, session_token, token_expires_at FROM users WHERE id = $1",
		userID)
	return db.rowToUser(row)
}

// GetUserByUsername gets a user by username.
func (db *Database) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, created_at, last_login, session_token, token_expires_at FROM users WHERE username = $1",
		username)
	return db.rowToUser(row)
}

// ListUsers returns all users ordered by username. Used by the admin CLI.
func (db *Database) ListUsers(ctx context.Context) ([]models.User, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT id, username, password_hash, role, created_at, last_login, session_token, token_expires_at FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.User, 0)
	for rows.Next() {
		var (
			id             uuid.UUID
			username       string
			passwordHash   string
			role           string
			createdAt      time.Time
			lastLogin      sql.NullTime
			sessionToken   sql.NullString
			tokenExpiresAt sql.NullTime
		)
		if err := rows.Scan(&id, &username, &passwordHash, &role, &createdAt, &lastLogin, &sessionToken, &tokenExpiresAt); err != nil {
			return nil, err
		}
		u := models.User{ID: id, Username: username, PasswordHash: passwordHash, Role: role, CreatedAt: createdAt}
		if lastLogin.Valid {
			u.LastLogin = &lastLogin.Time
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of provisioned users (the 0-users unprovisioned
// guard the admin CLI consults).
func (db *Database) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n)
	return n, err
}

// GetUserByToken gets a user by session token. Returns nil if token is expired.
func (db *Database) GetUserByToken(ctx context.Context, token string) (*models.User, error) {
	token = models.HashToken(token)
	row := db.conn.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, created_at, last_login, session_token, token_expires_at FROM users WHERE session_token = $1 AND (token_expires_at IS NULL OR token_expires_at > $2)",
		token, time.Now().UTC())
	return db.rowToUser(row)
}

func (db *Database) rowToUser(row *sql.Row) (*models.User, error) {
	var (
		id             uuid.UUID
		username       string
		passwordHash   string
		role           string
		createdAt      time.Time
		lastLogin      sql.NullTime
		sessionToken   sql.NullString
		tokenExpiresAt sql.NullTime
	)

	err := row.Scan(&id, &username, &passwordHash, &role, &createdAt, &lastLogin, &sessionToken, &tokenExpiresAt)
	if err != nil {
		return nil, err
	}

	user := &models.User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		Role:         role,
		CreatedAt:    createdAt,
	}
	if lastLogin.Valid {
		user.LastLogin = &lastLogin.Time
	}
	if sessionToken.Valid {
		user.SessionToken = &sessionToken.String
	}
	if tokenExpiresAt.Valid {
		user.TokenExpiresAt = &tokenExpiresAt.Time
	}
	return user, nil
}

// Task operations.
//
// The tasks-table column lists (taskColumns, taskInsertColumns,
// taskInsertOnConflict, the UPDATE statement) and scanTask's positional scan
// all derive from taskColumnRegistry — see task_columns.go (#1126). A new
// task column is one migration + one registry row (+ the models.Task field).

// sourceTaskIDValue maps the optional source-task lineage pointer (#270) to a
// nullable column value: nil → SQL NULL, set → the UUID string.
func sourceTaskIDValue(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// createdByTaskIDValue maps the optional spawned-by-task lineage pointer (#277)
// to a nullable column value: nil → SQL NULL, set → the UUID string. Mirrors
// sourceTaskIDValue; the two columns carry distinct lineage (re-run vs spawn).
func createdByTaskIDValue(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// recurrenceSpawnedInsertValue derives the recurrence spawn-settlement flag
// (#1116, migration 065) for a freshly INSERTED row. A row born success/error
// — restored history from `fleet import` (#713 preserves status/recurrence/
// completed_at verbatim), or any future insert of an already-completed row —
// must land SETTLED: its successor question was answered in the deployment it
// came from, and an unsettled flag would make the reconciliation sweep treat
// every restored occurrence of a recurring chain as a lost spawn and
// mass-spawn duplicate successors. Rows born in any other status stay FALSE:
// live rows settle through the normal spawn on their own success/error
// transition, cancelled rows never spawn (the sweep never selects them), and
// dead_lettered rows deliberately stay unsettled so a DLQ replay can continue
// the chain (see ReplayDeadLetteredTask). Like effective_priority, the column
// is insert-only here — it is excluded from the upsert/UpdateTaskTx so a
// status write can never clobber the spawn claim.
func recurrenceSpawnedInsertValue(t *models.Task) bool {
	return t.Status == models.TaskStatusSuccess || t.Status == models.TaskStatusError
}

// marshalTags serializes task tags for the JSONB column, ALWAYS as a JSON array
// (never the bare "null" marshalJSON emits for a nil slice) so the tags catalogue
// query's jsonb_array_elements_text never hits a scalar. Empty → "[]".
func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	return marshalJSON(tags)
}

// AddTask adds or updates a task via the registry-derived single-row upsert
// (taskInsertStatement, task_columns.go). The columns deliberately absent
// from the insert or its ON CONFLICT clause — effective_priority,
// recurrence_spawned, the result-like/pause/wake columns — are declared,
// with per-column reasons, on their taskColumnRegistry rows.
//
// taskInsertArgs populates actual_duration_seconds (#274) whenever a
// completion timestamp is present alongside a start, so EVERY write path
// that persists a completed_at also persists the derived actual, without
// each storage call site having to remember it. Idempotent: a pre-set value
// (e.g. a test seed) is left untouched.
func (db *Database) AddTask(ctx context.Context, task *models.Task) error {
	_, err := db.conn.ExecContext(ctx, taskInsertStatement, taskInsertArgs(task)...)
	return err
}

// serializationKeyValue maps the optional mutual-exclusion key (#709) to a
// nullable column value: nil/empty/whitespace-only → SQL NULL (unserialized),
// else the trimmed key. NewTask already normalizes, but re-normalizing here
// defends the claim gate against a directly-constructed Task (internal caller
// or test seed) that bypassed it — a NULL column can never serialize on "".
func serializationKeyValue(p *string) any {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*p)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

// recurrenceRemainingValue maps the optional remaining-runs counter to a
// nullable column value: nil → SQL NULL (unbounded), set → the int.
func recurrenceRemainingValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// thinkingBudgetValue maps the optional per-task thinking budget (#220) to a
// nullable column value: nil → SQL NULL (inherit the global default), else the
// int. Mirrors expectedDurationValue.
func thinkingBudgetValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// expectedDurationValue maps the optional expected-duration pointer (#274) to a
// nullable column value: nil → SQL NULL (no SLA), set → the int.
func expectedDurationValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// slaMultiplierValue defends the NOT NULL multiplier columns against a
// directly-constructed Task that bypassed NewTask (e.g. an internal caller or a
// test seed), where the field would otherwise be the zero value. 0/negative
// maps to the supplied default (matching NewTask's normalization); a positive
// value passes through verbatim.
func slaMultiplierValue(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// maybeComputeActualDuration sets task.ActualDurationSeconds from
// CompletedAt - StartedAt (whole seconds) when both are present and the field
// is not already populated (#274). It never overwrites a caller-set value so a
// test seed or an explicit write is preserved.
func maybeComputeActualDuration(task *models.Task) {
	if task.ActualDurationSeconds != nil {
		return
	}
	if task.StartedAt == nil || task.CompletedAt == nil {
		return
	}
	secs := int(task.CompletedAt.Sub(*task.StartedAt).Seconds())
	if secs < 0 {
		secs = 0
	}
	task.ActualDurationSeconds = &secs
}

// AddTaskBatch inserts a slice of tasks in a single parameterised INSERT (#227),
// replacing N sequential ExecContext round-trips. It does NOT run inside an
// explicit transaction — callers that need atomicity wrap the call in BeginTx /
// Commit (see Storage.AddTaskBatch). An empty slice is a no-op.
//
// Each row carries the SAME registry-derived columns as AddTask (via the
// shared taskInsertArgs helper), so a row inserted through the batch path is
// byte-identical to one inserted through the single-row path. The placeholder
// count is len(taskInsertSet) — derived, never hand-maintained (#710's drift
// class is structurally gone).
func (db *Database) AddTaskBatch(ctx context.Context, tasks []*models.Task) error {
	return db.AddTaskBatchTx(ctx, nil, tasks)
}

// AddTaskBatchTx inserts a slice of tasks in a single parameterised INSERT within
// an existing transaction (#227), ensuring atomic multi-row insertions run in
// a single round-trip. An empty slice is a no-op.
func (db *Database) AddTaskBatchTx(ctx context.Context, tx *sql.Tx, tasks []*models.Task) error {
	if len(tasks) == 0 {
		return nil
	}

	cols := len(taskInsertSet)
	args := make([]any, 0, len(tasks)*cols)
	placeholders := make([]string, 0, len(tasks))
	var b strings.Builder
	for i, t := range tasks {
		base := i * cols
		b.Reset()
		b.WriteByte('(')
		for j := 0; j < cols; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "$%d", base+j+1)
		}
		b.WriteByte(')')
		placeholders = append(placeholders, b.String())
		args = append(args, taskInsertArgs(t)...)
	}

	var q strings.Builder
	q.WriteString("INSERT INTO tasks (")
	q.WriteString(taskInsertColumns)
	q.WriteString(") VALUES ")
	q.WriteString(strings.Join(placeholders, ","))
	q.WriteString(taskInsertOnConflict)

	var err error
	if tx != nil {
		_, err = tx.ExecContext(ctx, q.String(), args...)
	} else {
		_, err = db.conn.ExecContext(ctx, q.String(), args...)
	}
	return err
}

// AddTaskTx inserts a single task within an existing transaction. The atomic
// batch path (#227) uses this so a multi-row insert lands in the caller's tx.
// It executes the same registry-derived taskInsertStatement as AddTask.
func (db *Database) AddTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	_, err := tx.ExecContext(ctx, taskInsertStatement, taskInsertArgs(task)...)
	return err
}

// deref returns the pointed-to string, or "" for a nil pointer. Paired with
// nullableString so a nil/empty DeadLetterReason persists as SQL NULL (#253).
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// deadLetterAttemptsValue maps the dead-letter attempt count (#253) to a nullable
// column value: 0 (the not-quarantined sentinel) → SQL NULL, >0 → the int. Keeps
// non-dead-lettered rows NULL in the column rather than a misleading 0.
func deadLetterAttemptsValue(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

// workspacePathValue maps the optional per-run workspace path (#287) to a
// nullable column value: nil/empty → SQL NULL, set → the path string.
func workspacePathValue(p *string) any {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return *p
}

// triggerTypeOrCron defends the NOT NULL trigger_type column against a
// directly-constructed Task that bypassed NewTask, where the field would
// otherwise be the empty string.
func triggerTypeOrCron(t models.TriggerType) string {
	if t == "" {
		return string(models.TriggerTypeCron)
	}
	return string(t)
}

// triggerTypeOrCronStr normalizes a scanned trigger_type, defaulting an empty
// value to "cron" (the column is NOT NULL DEFAULT 'cron', so this only guards
// against a stray empty string).
func triggerTypeOrCronStr(s string) string {
	if s == "" {
		return string(models.TriggerTypeCron)
	}
	return s
}

// taskTimezoneOrUTC defends the NOT NULL timezone column against a
// directly-constructed Task that bypassed NewTask (e.g. an internal caller),
// where the field would otherwise be the empty string.
func taskTimezoneOrUTC(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

func mcpSelectionOrEmpty(s models.MCPSelection) models.MCPSelection {
	if s == nil {
		return models.MCPSelection{}
	}
	return s
}

// effectivePriorityValue defends the NOT NULL effective_priority column (#230)
// against a directly-constructed Task that bypassed NewTask, where the field
// would be the Go zero value 0 — which under the ASC claim ordering is the MOST
// urgent. It falls back to the submitted Priority, then to Normal, so such a
// task is never silently dispatched ahead of everything else.
func effectivePriorityValue(t *models.Task) int {
	if t.EffectivePriority > 0 {
		return t.EffectivePriority
	}
	if t.Priority > 0 {
		return t.Priority
	}
	return models.PriorityNormal
}

// marshalCredentialAllowlist serializes the allowlist for the nullable JSONB
// column, PRESERVING the nil-vs-empty distinction: nil → SQL NULL ("inherit
// global"), a non-nil (possibly empty) list → its JSON ("[]" = deny all).
func marshalCredentialAllowlist(al models.CredentialAllowlist) any {
	if al == nil {
		return nil
	}
	return marshalJSON(al)
}

// unmarshalCredentialAllowlist reads the nullable JSONB column back. A NULL/empty
// column is nil ("inherit global"); "[]" decodes to a non-nil empty slice
// ("deny all"), so the distinction round-trips.
func unmarshalCredentialAllowlist(ns sql.NullString) models.CredentialAllowlist {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var result models.CredentialAllowlist
	if err := json.Unmarshal([]byte(ns.String), &result); err != nil {
		log.Printf("Warning: failed to unmarshal credential_allowlist: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return result
}

// marshalLoopConfig serializes the optional loop config for the nullable JSONB
// column: nil → SQL NULL (an ordinary one-shot task), non-nil → its JSON.
func marshalLoopConfig(lc *models.LoopConfig) any {
	if lc == nil {
		return nil
	}
	return marshalJSON(lc)
}

// unmarshalLoopConfig reads the nullable loop_config column back. NULL/empty → nil.
func unmarshalLoopConfig(ns sql.NullString) *models.LoopConfig {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var lc models.LoopConfig
	if err := json.Unmarshal([]byte(ns.String), &lc); err != nil {
		log.Printf("Warning: failed to unmarshal loop_config: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &lc
}

// marshalWorktreeConfig serializes the optional worktree config for the nullable
// JSONB column: nil → SQL NULL (shared-workspace task), non-nil → its JSON (#180).
func marshalWorktreeConfig(wc *models.WorktreeConfig) any {
	if wc == nil {
		return nil
	}
	return marshalJSON(wc)
}

// unmarshalWorktreeConfig reads the nullable worktree_config column back. NULL/empty → nil.
func unmarshalWorktreeConfig(ns sql.NullString) *models.WorktreeConfig {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var wc models.WorktreeConfig
	if err := json.Unmarshal([]byte(ns.String), &wc); err != nil {
		log.Printf("Warning: failed to unmarshal worktree_config: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &wc
}

// marshalSandboxLimits serializes the optional per-task sandbox limits for the
// nullable JSONB column: nil → SQL NULL (use the global ceilings), non-nil → its
// JSON (#205).
func marshalSandboxLimits(l *models.TaskSandboxLimits) any {
	if l == nil {
		return nil
	}
	return marshalJSON(l)
}

// unmarshalSandboxLimits reads the nullable sandbox_limits column back. NULL/empty → nil.
func unmarshalSandboxLimits(ns sql.NullString) *models.TaskSandboxLimits {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var l models.TaskSandboxLimits
	if err := json.Unmarshal([]byte(ns.String), &l); err != nil {
		log.Printf("Warning: failed to unmarshal sandbox_limits: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &l
}

// marshalRawJSON serializes an optional raw-JSON column value (output_schema /
// output_json, #244) for the nullable JSONB column: nil/empty → SQL NULL,
// non-nil → the bytes verbatim (already valid JSON — validated upstream).
func marshalRawJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// unmarshalRawJSON reads a nullable raw-JSON column back. NULL/empty → nil so the
// omitempty field stays absent (#244).
func unmarshalRawJSON(ns sql.NullString) json.RawMessage {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	return json.RawMessage(ns.String)
}

// marshalRetryPolicy serializes the optional retry policy for the nullable JSONB
// column: nil → SQL NULL (legacy policy), non-nil → its JSON (#201).
func marshalRetryPolicy(rp *models.RetryPolicy) any {
	if rp == nil {
		return nil
	}
	return marshalJSON(rp)
}

// unmarshalRetryPolicy reads the nullable retry_policy column back. NULL/empty → nil.
func unmarshalRetryPolicy(ns sql.NullString) *models.RetryPolicy {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var rp models.RetryPolicy
	if err := json.Unmarshal([]byte(ns.String), &rp); err != nil {
		log.Printf("Warning: failed to unmarshal retry_policy: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &rp
}

// marshalRunIf serializes the optional pre-run shell gate (#269) for the nullable
// JSONB column: nil → SQL NULL (the legacy unconditional promotion path), non-nil
// → its JSON.
func marshalRunIf(r *models.RunIf) any {
	if r == nil {
		return nil
	}
	return marshalJSON(r)
}

// unmarshalRunIf reads the nullable run_if column back. NULL/empty → nil.
func unmarshalRunIf(ns sql.NullString) *models.RunIf {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var r models.RunIf
	if err := json.Unmarshal([]byte(ns.String), &r); err != nil {
		log.Printf("Warning: failed to unmarshal run_if: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &r
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// scanTask scans one tasks row into a models.Task. The scan destinations and
// the per-column conversions both come from taskColumnRegistry's read set —
// the SAME ordered slice taskColumns is joined from — so the SELECT list and
// the positional scan agree by construction (no manual ordering to drift).
// Hot path: per row it fills one destination slice and runs the assign
// functions; all statement text was built once at package init.
func (db *Database) scanTask(scanner interface{ Scan(...interface{}) error }) (*models.Task, error) {
	var buf taskScanBuf
	dests := make([]any, len(taskReadSet))
	for i, c := range taskReadSet {
		dests[i] = c.dest(&buf)
	}
	if err := scanner.Scan(dests...); err != nil {
		return nil, err
	}
	task := &models.Task{}
	for _, c := range taskReadSet {
		c.assign(&buf, task)
	}
	return task, nil
}

func (db *Database) rowsToTasks(rows *sql.Rows) ([]*models.Task, error) {
	tasks := make([]*models.Task, 0)
	for rows.Next() {
		task, err := db.scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetTask gets a task by ID.
func (db *Database) GetTask(ctx context.Context, taskID uuid.UUID) (*models.Task, error) {
	row := db.conn.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = $1", taskID)
	return db.scanTask(row)
}

// TaskExists reports whether a task row with the given id exists. Used by the
// legacy importer (docs/LEGACY-IMPORT.md) to make re-runs skip-by-default: a
// UUID already present in fleet is never overwritten unless the operator
// passes --overwrite, so a re-run can't revert live task state (#713).
func (db *Database) TaskExists(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM tasks WHERE id = $1)", taskID).Scan(&exists)
	return exists, err
}

// GetAllTasks gets all tasks.
func (db *Database) GetAllTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetScheduledTasks gets scheduled tasks that are due to run up to a limit,
// strictly after the (afterScheduledFor, afterID) keyset cursor in the total
// order (scheduled_for ASC, id ASC). Pass the zero time.Time / uuid.Nil to
// start from the beginning.
//
// Keyset pagination (not plain LIMIT) is what lets the scheduler page past
// soft-held rows that stay in the due set (#566): a row whose run_if gate
// declines keeps its scheduled_for, so with LIMIT-only paging a full page of
// held rows re-fetched identically forever. The tiebreaking `id` column
// matters: scheduled_for alone is not a total order, so a LIMIT prefix over it
// is not stable across queries and rows tied at the boundary could be masked
// indefinitely within one pass sequence.
func (db *Database) GetScheduledTasks(ctx context.Context, cutoff time.Time, afterScheduledFor time.Time, afterID uuid.UUID, limit int) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		AND scheduled_for IS NOT NULL
		AND scheduled_for <= $2
		AND trigger_type = 'cron'
		AND (scheduled_for, id) > ($3, $4)
		ORDER BY scheduled_for ASC, id ASC
		LIMIT $5`,
		string(models.TaskStatusScheduled), cutoff, afterScheduledFor, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// UpdateTask updates an existing task.
func (db *Database) UpdateTask(ctx context.Context, task *models.Task) error {
	return db.AddTask(ctx, task)
}

// UpdateTasksModelBatch updates the pinned model of scheduled tasks.
// fallbackModel is optional: nil leaves existing fallback_model values
// untouched; a non-nil empty string clears them to NULL; a non-nil
// non-empty string sets them. Callers must distinguish "flag not
// provided" from "explicitly clear" (#1120).
func (db *Database) UpdateTasksModelBatch(ctx context.Context, model string, fallbackModel *string, fromModel string) (int, error) {
	var res sql.Result
	var err error
	status := string(models.TaskStatusScheduled)
	switch {
	case fallbackModel == nil && fromModel != "":
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1
			WHERE status = $2 AND model = $3`,
			model, status, fromModel)
	case fallbackModel == nil:
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1
			WHERE status = $2`,
			model, status)
	case fromModel != "":
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3 AND model = $4`,
			model, nullableString(*fallbackModel), status, fromModel)
	default:
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3`,
			model, nullableString(*fallbackModel), status)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetAllScheduledTasks returns all scheduled tasks regardless of due time.
func (db *Database) GetAllScheduledTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		ORDER BY scheduled_for ASC`,
		string(models.TaskStatusScheduled))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// ListTasksForExport returns task definitions for GET /tasks/export (#238). It
// is a complete snapshot (no pagination) so the caller can download the whole
// file. ids, when non-empty, limits the result to those task IDs (the ?ids=
// filter); an empty slice exports every task. recurrenceOnly, when true,
// restricts the result to tasks with a non-empty recurrence (cron tasks only —
// the ?recurrence_only=true filter). Ordered by created_at for a stable diff.
func (db *Database) ListTasksForExport(ctx context.Context, ids []uuid.UUID, recurrenceOnly bool) ([]*models.Task, error) {
	q := "SELECT " + taskColumns + " FROM tasks WHERE 1=1"
	args := []any{}
	if len(ids) > 0 {
		q += " AND id = ANY($1::uuid[])"
		args = append(args, uuidStrings(ids))
	}
	if recurrenceOnly {
		q += " AND COALESCE(recurrence, '') <> ''"
	}
	q += " ORDER BY created_at ASC, id ASC"
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// FindTaskIDsByName resolves task IDs by non-empty name (#238). It is the
// pre-flight conflict-detection query for POST /tasks/import: a name present in
// the returned map collides with an existing task. Empty names are never
// matched (they cannot collide by name). Names are matched case-sensitively.
func (db *Database) FindTaskIDsByName(ctx context.Context, names []string) (map[string]uuid.UUID, error) {
	out := make(map[string]uuid.UUID)
	var filtered []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == 0 {
		return out, nil
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, name FROM tasks
		WHERE name = ANY($1::text[]) AND name <> ''`,
		filtered)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

// GetTaskByName returns the task whose non-empty name matches, or (nil, nil)
// when no such task exists. Used by import conflict=replace to fetch the row to
// update in place (#238).
func (db *Database) GetTaskByName(ctx context.Context, name string) (*models.Task, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	row := db.conn.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE name = $1 AND name <> ''", name)
	t, err := db.scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// UpdateTasksStatusBatch transitions tasks from fromStatus to toStatus, skipping
// any that have left fromStatus. Returns the number transitioned.
func (db *Database) UpdateTasksStatusBatch(ctx context.Context, taskIDs []uuid.UUID, fromStatus, toStatus models.TaskStatus) (int, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = $1
		WHERE id = ANY($2::uuid[]) AND status = $3`,
		string(toStatus),
		uuidStrings(taskIDs),
		string(fromStatus),
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// SettleGatedTask transitions one gated task out of `scheduled` to toStatus,
// but ONLY while the row still carries the scheduled_for the gate evaluation
// was dispatched against (NULL-safe compare). The status check alone is not
// enough: an edit or reschedule keeps the task `scheduled`, and since gates
// settle asynchronously (up to their full 300s runtime later), a stale
// verdict conditioned only on status would either run a task an operator had
// just postponed or clobber the operator's new scheduled_for. A task
// cancelled, claimed, edited, or rescheduled while its gate ran fails the
// WHERE and the verdict is discarded — the next due tick re-evaluates the
// task's current definition. Returns the number of rows transitioned (0 or 1).
func (db *Database) SettleGatedTask(ctx context.Context, taskID uuid.UUID, observedScheduledFor *time.Time, toStatus models.TaskStatus) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = $1
		WHERE id = $2 AND status = $3 AND scheduled_for IS NOT DISTINCT FROM $4`,
		string(toStatus),
		taskID,
		string(models.TaskStatusScheduled),
		observedScheduledFor,
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// GetPendingTasks gets all pending tasks, ordered the same way the claim path
// dispatches them: effective_priority ASC (lower = more urgent, #230), then
// created_at ASC (FIFO within a tier).
func (db *Database) GetPendingTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		ORDER BY effective_priority ASC, created_at ASC`,
		string(models.TaskStatusPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// taskActiveStatuses are the statuses that HOLD a serialization key (#709): the
// task is (about to be) executing, so a same-key pending task must not start.
// Mirrors GetRunningTasks' definition of "in flight". paused_awaiting_input is
// deliberately NOT active: a paused run has stopped executing (its lease is
// released), and a resume re-queues the task as pending, which re-passes this
// gate before it can run again.
func taskActiveStatuses() []any {
	return []any{
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
	}
}

// serializationNotBlockedSQL filters out pending tasks whose serialization key
// is currently held by an active task, so a blocked task does not consume the
// claim's single candidate slot (the claim query is LIMIT 1 — without this, a
// blocked task at the head of the queue would starve every task behind it).
// This filter is best-effort VISIBILITY only; the correctness guarantee is the
// advisory-lock re-check in ClaimNextPendingTask, which runs under the per-key
// lock at claim time. $2–$3 are the active statuses (taskActiveStatuses).
const serializationNotBlockedSQL = `(
			tasks.serialization_key IS NULL
			OR NOT EXISTS (
				SELECT 1 FROM tasks blocked
				WHERE blocked.serialization_key = tasks.serialization_key
				AND blocked.id <> tasks.id
				AND blocked.status IN ($2, $3)
			)
		)`

// acquireSerializationLockTx takes a transaction-scoped advisory lock on the
// given serialization key (#709). It serializes concurrent same-key claim
// attempts DB-wide: two transactions claiming tasks with the same key execute
// their active-task existence check strictly one after the other, so both
// cannot pass it simultaneously. Released automatically at commit/rollback
// (pg_advisory_xact_lock), so callers never unlock explicitly. hashtext
// collisions across distinct keys are possible and harmless: they only make
// two unrelated claims briefly serialize, never interleave.
func acquireSerializationLockTx(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", key)
	return err
}

// hasActiveTaskWithSerializationKeyTx reports whether any task other than
// excludeTaskID holds the given serialization key in an active state. Must be
// called within a transaction, after acquireSerializationLockTx, for the
// answer to be race-free.
func hasActiveTaskWithSerializationKeyTx(ctx context.Context, tx *sql.Tx, key string, excludeTaskID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM tasks
			WHERE serialization_key = $1
			AND id <> $2
			AND status IN ($3, $4)
		)`,
		append([]any{key, excludeTaskID}, taskActiveStatuses()...)...,
	).Scan(&exists)
	return exists, err
}

// ClaimNextPendingTask atomically claims the next pending task for the given
// lease owner using FOR UPDATE SKIP LOCKED, so two concurrent workers never
// claim the same row and a row another worker holds is skipped rather than
// blocked on. It leases the task (status=leased, lease_owner=owner,
// lease_expires_at=now+leaseDuration) inside one transaction and returns the
// claimed task, or (nil, nil) when no pending task is available.
//
// This is the in-process worker's claim path. It replaces moc's
// node-targeted AssignTaskToNode for the runner: there is one synthetic
// in-box lease owner, no node routing, no glob matching.
//
// Serialization gate (#709, moc#442 parity): at most one task per
// serialization_key may be active (leased/running) at a time. The
// candidate SELECT filters out visibly-blocked tasks (best-effort, so a
// blocked head-of-queue task never starves the tasks behind it), and a
// candidate that DOES carry a key is re-checked under a transaction-scoped
// per-key advisory lock before the lease is written — that locked re-check is
// the correctness guarantee against two same-key claims racing each other. A
// blocked candidate is declined (nil, nil), stays pending untouched, and is
// retried on a later claim pass — skipped, never failed. This claim is the
// ONLY pending→active transition (requeue/recovery/resume all re-queue to
// pending), so every path to execution re-passes this gate.
func (db *Database) ClaimNextPendingTask(ctx context.Context, leaseOwner string, leaseDuration time.Duration) (*models.Task, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	// SKIP LOCKED: skip rows a concurrent claim already locked rather than
	// blocking, so two workers polling at once each get a distinct task.
	row := tx.QueryRowContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		AND `+serializationNotBlockedSQL+`
		ORDER BY effective_priority ASC, created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
		append([]any{string(models.TaskStatusPending)}, taskActiveStatuses()...)...)
	task, err := db.scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Locked re-check for serialized tasks: the visibility filter above ran
	// without the per-key lock, so two same-key candidates claimed by two
	// concurrent transactions could both have passed it. Under the advisory
	// lock the existence check is race-free: the loser blocks until the winner
	// commits its lease, then sees the now-active row and declines.
	if task.SerializationKey != nil {
		if err := acquireSerializationLockTx(ctx, tx, *task.SerializationKey); err != nil {
			return nil, err
		}
		blocked, err := hasActiveTaskWithSerializationKeyTx(ctx, tx, *task.SerializationKey, task.ID)
		if err != nil {
			return nil, err
		}
		if blocked {
			// Another same-key task is active: decline the claim. The rollback
			// releases the row lock and the task stays pending for a later pass.
			return nil, nil
		}
	}

	now := time.Now().UTC()
	expiresAt := now.Add(leaseDuration)
	task.Status = models.TaskStatusLeased
	task.LeaseOwner = &leaseOwner
	task.LeaseExpiresAt = &expiresAt
	// StartedAt is deliberately NOT set here; it is set on the first running update.

	if err := db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// PromoteStarvedTasks is the anti-starvation sweep (#230): any pending task that
// has waited longer than windowMinutes and is still LESS urgent than the
// starvation floor has its effective_priority raised TO that floor (never its
// submitted priority), so a sustained stream of higher-priority work can't keep
// it queued forever. The floor is High, never Critical, so relief for a starving
// batch task can't preempt genuinely critical work. windowMinutes <= 0 disables
// the sweep (no-op). Returns the number of tasks promoted.
func (db *Database) PromoteStarvedTasks(ctx context.Context, windowMinutes int) (int64, error) {
	if windowMinutes <= 0 {
		return 0, nil
	}
	// Compute the age cutoff in Go and compare against the TIMESTAMPTZ column
	// directly — avoids any driver-specific interval-parameter typing.
	cutoff := time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute)
	// Measure the wait from when the task became eligible to run, not from
	// created_at. A recurring occurrence ROW is created at the PREVIOUS
	// occurrence's completion, so by the time it flips to pending its created_at
	// is already ~one period old — keying the sweep on created_at would
	// floor-promote every recurring/retried task the instant it becomes pending,
	// inverting the priority queue exactly when the operator enables the window.
	// GREATEST(created_at, scheduled_for) is that eligibility time: a fresh
	// recurrence's scheduled_for is its (near-now) fire time, a retry's is the
	// backoff time, and a resume bumps scheduled_for to now() — so none are
	// mis-promoted. created_at is never NULL, so Postgres GREATEST simply ignores
	// a NULL scheduled_for (immediate tasks) and falls back to created_at.
	// (A crash-recovered task keeps its old scheduled_for and so is promoted
	// immediately — intended: recovered work should not lose more ground to
	// freshly-queued bulk work.)
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks
		SET effective_priority = $1
		WHERE status = $2
		  AND priority > $1
		  AND effective_priority > $1
		  AND GREATEST(created_at, scheduled_for) < $3`,
		models.StarvationFloorPriority, string(models.TaskStatusPending), cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ExpirePausedTasks fails tasks that have sat in paused_awaiting_input past the
// window (#510) — an unattended ask-pause otherwise waits forever. A non-positive
// window is a no-op (the default — waits forever). It returns the rows it moved
// to the terminal state so the caller can spawn the next occurrence for recurring
// tasks (see Storage.ExpirePausedTasks).
//
// It moves them to the TERMINAL `error` status (not dead_lettered, which is the
// runner's lease-guarded status by convention — a paused task holds no lease),
// stamping completed_at + error_message and clearing the pending question so
// the row reads as a clean terminal failure. Age is measured from paused_at
// (#1116) — the instant PauseTaskForQuestion parked the task. It used to be
// measured from started_at, "acceptably conservative" for short runs, but a
// run that executed 2h before asking under a 60-minute window was expired on
// the next tick: a zero TTL. Migration 064 backfills paused_at from started_at
// for rows already paused at upgrade time; a paused row with NULL paused_at
// (no in-repo writer produces one) is deliberately never expired — failing
// open to "waits forever", the sweep's own disabled-window default.
func (db *Database) ExpirePausedTasks(ctx context.Context, windowMinutes int) ([]*models.Task, error) {
	if windowMinutes <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute)
	// UPDATE ... RETURNING makes the terminal transition AND the capture of which
	// rows transitioned atomic under one lock acquisition: a concurrent ResumeTask
	// (guarded by `status='paused_awaiting_input'`) either commits first (this
	// WHERE then excludes the row) or blocks and wakes to find status='error' —
	// so a row is never both resumed and expired, and each expired row is returned
	// exactly once. The caller spawns the next recurrence for returned recurring
	// rows (see Storage.ExpirePausedTasks); without that, an expired occurrence of
	// a recurring task would silently end the whole schedule.
	rows, err := db.conn.QueryContext(ctx, `
		UPDATE tasks
		SET status = $1, completed_at = now(), error_message = $2, pending_question = NULL
		WHERE status = $3
		  AND paused_at IS NOT NULL
		  AND paused_at < $4
		RETURNING `+taskColumns,
		string(models.TaskStatusError),
		fmt.Sprintf("expired: awaited input for more than %d minute(s) with no answer", windowMinutes),
		string(models.TaskStatusPausedAwaitingInput), cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var expired []*models.Task
	for rows.Next() {
		t, serr := db.scanTask(rows)
		if serr != nil {
			return nil, serr
		}
		expired = append(expired, t)
	}
	return expired, rows.Err()
}

// GetUnspawnedRecurringTasks returns terminal recurring occurrences whose
// next-occurrence spawn is still unsettled (#1116): status success/error (the
// only statuses that spawn — cancel and dead-letter deliberately end/park the
// chain), a non-empty recurrence, recurrence_spawned still FALSE, and
// completed_at older than olderThan (a grace window so the sweep never races
// the normal post-commit spawn that is usually milliseconds behind the
// terminal commit — the guarded spawn is idempotent regardless, this just
// avoids pointless contention). Ordered oldest-first and bounded by limit so
// one sweep can never balloon a tick. Backed by idx_tasks_recurrence_unspawned
// (migration 065).
func (db *Database) GetUnspawnedRecurringTasks(ctx context.Context, olderThan time.Time, limit int) ([]*models.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)
		  AND recurrence IS NOT NULL AND recurrence <> ''
		  AND NOT recurrence_spawned
		  AND completed_at IS NOT NULL
		  AND completed_at < $3
		ORDER BY completed_at ASC
		LIMIT $4`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// PendingQueueStats returns the per-effective-priority rollup of the pending
// queue (#230): the count and the longest wait (seconds) at each distinct
// effective_priority. The handler aggregates these into named tiers for
// GET /admin/queue.
func (db *Database) PendingQueueStats(ctx context.Context) ([]models.QueuePriorityBucket, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT effective_priority,
		       COUNT(*),
		       COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(created_at)))::bigint, 0)
		FROM tasks
		WHERE status = $1
		GROUP BY effective_priority
		ORDER BY effective_priority ASC`,
		string(models.TaskStatusPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.QueuePriorityBucket, 0)
	for rows.Next() {
		var b models.QueuePriorityBucket
		var ageSeconds int64
		if err := rows.Scan(&b.Priority, &b.Count, &ageSeconds); err != nil {
			return nil, err
		}
		b.OldestAgeSeconds = int(ageSeconds)
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetRunningTasks gets all currently running tasks.
func (db *Database) GetRunningTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)`,
		string(models.TaskStatusRunning),
		string(models.TaskStatusLeased))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetTasksByStatus gets all tasks with a specific status.
func (db *Database) GetTasksByStatus(ctx context.Context, status models.TaskStatus) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status = $1", string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetDeadLetteredTasks returns dead-lettered tasks (#253), ordered by when they
// entered the queue (newest first) for the DLQ review listing. A non-positive
// limit returns every matching row; otherwise limit/offset paginate. The partial
// index on dead_lettered_at (migration 034) backs the ORDER BY.
func (db *Database) GetDeadLetteredTasks(ctx context.Context, limit, offset int) ([]*models.Task, error) {
	query := "SELECT " + taskColumns + " FROM tasks WHERE status = $1 ORDER BY dead_lettered_at DESC NULLS LAST"
	args := []any{string(models.TaskStatusDeadLettered)}
	if limit > 0 {
		query += " LIMIT $2 OFFSET $3"
		args = append(args, limit, offset)
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetRunningTasksWithSLA returns the in-flight tasks that carry an SLA
// (expected_duration_minutes IS NOT NULL) for the SLA monitor goroutine (#274).
// "In-flight" mirrors GetRunningTasks: leased / running — the
// statuses where StartedAt is set and the task has not yet reached a terminal
// state. The partial index idx_tasks_sla does NOT cover this query (it is
// keyed on completed_at), but the in-flight set is small (one host, capped
// pool) so a seq scan filtered by status + expected_duration_minutes IS NOT
// NULL is cheap; an extra index would not pay for itself.
func (db *Database) GetRunningTasksWithSLA(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2)
		AND expected_duration_minutes IS NOT NULL`,
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// MarkSLABreached latches sla_breached=true on a task the SLA monitor flagged
// as having crossed its fail threshold (#274). It is a narrow, single-column
// UPDATE so it cannot race a concurrent terminal-status write on the broader
// row. Idempotent: setting true on an already-breached row is a no-op.
func (db *Database) MarkSLABreached(ctx context.Context, taskID uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE tasks SET sla_breached = TRUE WHERE id = $1`, taskID)
	return err
}

// PauseTaskForQuestion parks a RUNNING task in paused_awaiting_input with the
// agent's question (#510), clearing the lease so the paused task holds no
// sandbox/container. Guarded on the caller's lease so a recovered run can't
// pause a task it no longer owns. Returns whether it applied. pending_answer
// is nulled alongside the new question: since #582 the runner clears the Q&A
// columns only at a terminal transition, so a resumed run that pauses AGAIN
// would otherwise leave the prior answer dangling next to the new question.
// paused_at stamps the pause instant (#1116) — ExpirePausedTasks counts the
// ask window from it, so a long run's question gets the full TTL instead of
// one already eroded by execution time.
func (db *Database) PauseTaskForQuestion(ctx context.Context, taskID, leaseOwner uuid.UUID, question string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'paused_awaiting_input', pending_question = $1, pending_answer = NULL,
			paused_at = now(),
			lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $2 AND lease_owner = $3 AND status = 'running'`,
		question, taskID, leaseOwner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResumeTask answers a paused task's question and re-queues it (#510): status →
// pending, pending_answer set, scheduled_for = now so it is immediately
// claimable. Guarded on the paused status. Returns whether it applied.
func (db *Database) ResumeTask(ctx context.Context, taskID uuid.UUID, answer string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', pending_answer = $1, scheduled_for = now()
		WHERE id = $2 AND status = 'paused_awaiting_input'`,
		answer, taskID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PauseTaskForWake parks a RUNNING task in paused_awaiting_wake (self-wake,
// docs/SELF-WAKE.md), clearing the lease so the parked task holds no
// sandbox/container — the exact shape of PauseTaskForQuestion, keyed on a
// deadline/event instead of a human. wake_at is ALWAYS set (a timer sleep's
// fire time, or an event wait's timeout deadline), so the wake sweep is the
// only expiry mechanism needed. wake_reason is nulled: like pending_answer,
// it belongs to the wake that has not happened yet. wake_cycles increments
// here, under the same guarded write, so the runner's cycle cap can't be
// raced past. Guarded on the caller's lease; returns whether it applied.
// paused_at stamps the park instant (#1116) for one consistent "entered its
// pause" record across both parked states; the wake expiry itself stays
// wake_at-driven (wake_at is ALWAYS set), so paused_at joins no wake predicate.
func (db *Database) PauseTaskForWake(ctx context.Context, taskID, leaseOwner uuid.UUID, wakeAt time.Time, eventKey, note string) (bool, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'paused_awaiting_wake',
			wake_at = $1, wake_event_key = NULLIF($2, ''), wake_note = $3, wake_reason = NULL,
			wake_cycles = wake_cycles + 1,
			paused_at = now(),
			lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $4 AND lease_owner = $5 AND status = 'running'`,
		wakeAt.UTC(), eventKey, note, taskID, leaseOwner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// WakeDueTasks re-queues every parked task whose wake deadline has passed:
// status → pending, scheduled_for = now so it is immediately claimable, and
// wake_reason records WHY it woke — a timer sleep's deadline fired, or an
// event wait timed out (the reason names the event so the resumed agent
// knows the event never arrived). Returns how many tasks it woke.
func (db *Database) WakeDueTasks(ctx context.Context) (int, error) {
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', scheduled_for = now(),
			wake_reason = CASE
				WHEN wake_event_key IS NOT NULL AND wake_event_key <> ''
					THEN 'timed out waiting for event "' || wake_event_key || '"'
				ELSE 'the sleep timer fired'
			END
		WHERE status = 'paused_awaiting_wake' AND wake_at IS NOT NULL AND wake_at <= now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// WakeTaskByEvent wakes ONE parked task early because its named event
// arrived (POST /tasks/{id}/wake). Guarded on the paused status AND the
// exact event key, so a wake with the wrong key (or against a timer-only
// sleep) is a no-op reported to the caller. note, when non-empty, is carried
// into the wake reason so the resumed agent sees the event payload's gist.
func (db *Database) WakeTaskByEvent(ctx context.Context, taskID uuid.UUID, eventKey, note string) (bool, error) {
	reason := `event "` + eventKey + `" fired`
	if note != "" {
		reason += ": " + note
	}
	res, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET status = 'pending', scheduled_for = now(), wake_reason = $1
		WHERE id = $2 AND status = 'paused_awaiting_wake' AND wake_event_key = $3`,
		reason, taskID, eventKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClearWakeState clears a woken task's wake columns once the resumed run has
// consumed them, under the run's lease — the wake counterpart of
// ClearPendingQA (wake_cycles deliberately survives: it is the lifetime
// park counter the cycle cap checks). Best-effort.
func (db *Database) ClearWakeState(ctx context.Context, taskID, leaseOwner uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET wake_at = NULL, wake_event_key = NULL, wake_note = NULL, wake_reason = NULL
		WHERE id = $1 AND lease_owner = $2`, taskID, leaseOwner)
	return err
}

// ClearPendingQA clears a resumed task's question+answer once the run has
// consumed them, under the run's lease so a stale writer can't wipe a fresh
// pause. Best-effort (the run proceeds regardless).
func (db *Database) ClearPendingQA(ctx context.Context, taskID, leaseOwner uuid.UUID) error {
	_, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET pending_question = NULL, pending_answer = NULL
		WHERE id = $1 AND lease_owner = $2`, taskID, leaseOwner)
	return err
}

// ListPausedTasks returns tasks awaiting a human answer (#510), newest first.
func (db *Database) ListPausedTasks(ctx context.Context, limit int) ([]*models.Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE status = 'paused_awaiting_input' ORDER BY started_at DESC NULLS LAST, created_at DESC LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*models.Task
	for rows.Next() {
		t, err := db.scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetErrorAnalysis persists the post-failure LLM diagnosis (#317) as a narrow,
// lease-FREE single-column UPDATE. It runs in a detached goroutine AFTER the
// terminal-failure transition (which already released the lease), so it
// deliberately does not check lease ownership.
//
// The status guard (... AND status IN error/dead_lettered) makes the write a
// no-op once the row is no longer in a terminal-failure state — specifically, if
// an admin REPLAYED the dead-lettered task (same id → pending → running) while
// this analysis goroutine was still in flight, the stale diagnosis is dropped
// rather than stamped onto the fresh attempt. Writing a diagnostic annotation to
// a still-terminal-failed row is benign (touches neither status nor lease) and,
// like MarkSLABreached, the single-column write cannot race a broader row write.
// nil/empty raw → SQL NULL. Idempotent.
func (db *Database) SetErrorAnalysis(ctx context.Context, taskID uuid.UUID, raw json.RawMessage) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE tasks SET error_analysis = $1 WHERE id = $2 AND status IN ($3, $4)`,
		marshalRawJSON(raw), taskID, string(models.TaskStatusError), string(models.TaskStatusDeadLettered))
	return err
}

// GetSLAReport aggregates the per-prompt SLA actuals over the last windowDays
// (#274): the p50/p95 actual run duration and the breach rate for each
// (prompt, expected_duration_minutes) bucket. Rows without an expected duration
// or an actual duration are excluded. windowDays is clamped to [1, 90]; the
// partial index idx_tasks_sla backs the WHERE filter. Buckets are ordered by
// breach rate (worst first) so the most violated SLAs surface at the top.
//
// The window uses make_interval(days => $1) rather than ($1 || ' days')::INTERVAL:
// the latter makes Postgres infer $1 as TEXT, which the pgx driver then refuses
// to encode a Go int into ("cannot find encode plan"), so the report errored for
// every caller. make_interval's days param is typed int, so the bound int4
// encodes cleanly. Do NOT revert to string concatenation (#458).
func (db *Database) GetSLAReport(ctx context.Context, windowDays int) (*models.SLAReport, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	if windowDays > 90 {
		windowDays = 90
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			-- Group by the operator's title when the job has one: every
			-- occurrence of a recurring task shares its title, so titled jobs
			-- collapse into one row per job instead of one per prompt variant.
			-- Untitled tasks keep the historical prompt grouping.
			COALESCE(NULLIF(title, ''), prompt)                      AS task_name,
			expected_duration_minutes,
			COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			CASE WHEN COUNT(*) = 0 THEN 0.0
			     ELSE 100.0 * SUM(CASE WHEN sla_breached THEN 1 ELSE 0 END) / COUNT(*) END,
			COUNT(*)
		FROM tasks
		WHERE completed_at >= NOW() - make_interval(days => $1)
		AND expected_duration_minutes IS NOT NULL
		AND actual_duration_seconds IS NOT NULL
		GROUP BY COALESCE(NULLIF(title, ''), prompt), expected_duration_minutes
		ORDER BY 5 DESC, 1 ASC`,
		windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &models.SLAReport{
		Period:     "last_" + strconv.Itoa(windowDays) + "_days",
		WindowDays: windowDays,
		Tasks:      []models.SLAReportTask{},
	}
	for rows.Next() {
		var (
			taskName    string
			expectedMin sql.NullInt64
			p50Min      sql.NullFloat64
			p95Min      sql.NullFloat64
			breachRate  sql.NullFloat64
			sampleCount sql.NullInt64
		)
		if err := rows.Scan(&taskName, &expectedMin, &p50Min, &p95Min, &breachRate, &sampleCount); err != nil {
			return nil, err
		}
		row := models.SLAReportTask{TaskName: taskName}
		if expectedMin.Valid {
			row.ExpectedMinutes = int(expectedMin.Int64)
		}
		if p50Min.Valid {
			row.P50ActualMinutes = p50Min.Float64
		}
		if p95Min.Valid {
			row.P95ActualMinutes = p95Min.Float64
		}
		if breachRate.Valid {
			// Round to 1 decimal place, mirroring the SQL ROUND(...,1) in the issue.
			row.BreachRatePercent = math.Round(breachRate.Float64*10) / 10
		}
		if sampleCount.Valid {
			row.SampleCount = int(sampleCount.Int64)
		}
		out.Tasks = append(out.Tasks, row)
	}
	return out, rows.Err()
}

// GetTasksCompletedToday gets tasks completed today.
func (db *Database) GetTasksCompletedToday(ctx context.Context) ([]*models.Task, error) {
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE completed_at BETWEEN $1 AND $2",
		todayStart, todayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetDashboardStats gets statistics for the dashboard.
func (db *Database) GetDashboardStats(ctx context.Context) (*models.DashboardStats, error) {
	stats := &models.DashboardStats{}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	err := db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = $1) as pending_tasks,
			COUNT(*) FILTER (WHERE status IN ($2, $3)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $4 AND completed_at BETWEEN $5 AND $6) as completed_today,
			COUNT(*) FILTER (WHERE status = $7 AND completed_at BETWEEN $5 AND $6) as failed_today
		FROM tasks`,
		string(models.TaskStatusPending),
		string(models.TaskStatusRunning),
		string(models.TaskStatusLeased),
		string(models.TaskStatusSuccess),
		todayStart,
		todayEnd,
		string(models.TaskStatusError),
	).Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasksToday, &stats.FailedTasksToday)
	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}
	return stats, nil
}

// Log operations

// runLogHistoryKeep bounds how many superseded transcripts run_logs retains
// per task (#history): the trim runs inside the same transaction as the
// copy-on-overwrite, so the cap can never be exceeded between sweeps.
const runLogHistoryKeep = 20

// archiveSupersededLog copies the task's CURRENT logs row (if any) into
// run_logs, then trims that task's history past runLogHistoryKeep. Called
// inside the AddLog/AddLogRaw transaction immediately before the upsert that
// would otherwise destroy the row — so history costs nothing for a task that
// only ever writes one transcript (retry-free, never resumed). The columns
// travel verbatim: an archived (gz+codec) payload stays archived.
func archiveSupersededLog(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_logs (task_id, session_data, session_data_gz, session_compression)
		SELECT task_id, session_data, session_data_gz, session_compression
		FROM logs WHERE task_id = $1`, taskID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM run_logs WHERE task_id = $1 AND id NOT IN (
			SELECT id FROM run_logs WHERE task_id = $1
			ORDER BY superseded_at DESC, id DESC LIMIT $2
		)`, taskID, runLogHistoryKeep)
	return err
}

// upsertLog is the shared write path of AddLog/AddLogRaw: archive the row the
// upsert would clobber (per-attempt history), then write the new payload live
// (plaintext JSON in session_data); the archival columns are reset so a
// re-write of a previously archived row returns it to the live, uncompressed
// state.
func (db *Database) upsertLog(ctx context.Context, taskID uuid.UUID, sessionJSON []byte) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := archiveSupersededLog(ctx, tx, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO logs (task_id, session_data, session_data_gz, session_compression)
		VALUES ($1, $2, NULL, NULL)
		ON CONFLICT (task_id) DO UPDATE SET
			session_data = EXCLUDED.session_data,
			session_data_gz = NULL,
			session_compression = NULL`,
		taskID, string(sessionJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

// AddLog stores a log session for a task, archiving any transcript it
// supersedes into run_logs first (per-attempt history).
func (db *Database) AddLog(ctx context.Context, taskID uuid.UUID, session *models.LogSession) error {
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return db.upsertLog(ctx, taskID, sessionJSON)
}

// AddLogRaw stores a pre-serialized log session verbatim (legacy import,
// docs/LEGACY-IMPORT.md). The payload travels byte-for-byte from the source
// system's logs.session_data — no unmarshal/remarshal round-trip that could
// drop fields a newer/older LogSession shape doesn't know about. Same
// archive-then-upsert semantics as AddLog.
func (db *Database) AddLogRaw(ctx context.Context, taskID uuid.UUID, sessionJSON []byte) error {
	return db.upsertLog(ctx, taskID, sessionJSON)
}

// LogExists reports whether a run-log row exists for the task. Used by the
// legacy importer's skip-by-default re-run posture (#713), mirroring TaskExists.
func (db *Database) LogExists(ctx context.Context, taskID uuid.UUID) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM logs WHERE task_id = $1)", taskID).Scan(&exists)
	return exists, err
}

// decodeLogRow turns one logs row into JSON bytes, transparently inflating (and
// decrypting, when a key is configured) an archived payload (#272). Exactly one
// of sessionData / gz is populated: a live row carries plaintext in sessionData
// with an empty codec; an archived row carries bytes in gz with a non-empty
// codec and a NULL sessionData.
func (db *Database) decodeLogRow(sessionData *string, gz []byte, codec string) ([]byte, error) {
	if codec != "" {
		return decodeArchive(gz, db.archiveKey, codec)
	}
	if sessionData != nil {
		return []byte(*sessionData), nil
	}
	return nil, errors.New("log row has neither live nor archived payload")
}

// GetLog gets the log session for a task, transparently inflating an archived
// payload so callers see no difference between live and archived logs (#272).
func (db *Database) GetLog(ctx context.Context, taskID uuid.UUID) (*models.LogSession, error) {
	var sessionData *string
	var gz []byte
	var codec sql.NullString
	err := db.conn.QueryRowContext(ctx,
		"SELECT session_data, session_data_gz, session_compression FROM logs WHERE task_id = $1",
		taskID).Scan(&sessionData, &gz, &codec)
	if err != nil {
		return nil, err
	}
	raw, err := db.decodeLogRow(sessionData, gz, codec.String)
	if err != nil {
		return nil, err
	}
	var session models.LogSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// ListRunLogHistory lists a task's superseded transcripts (per-attempt
// history), newest first: id + when each was superseded. The payloads are
// fetched one at a time via GetRunLogEntry — a history listing must never
// drag every archived transcript across the wire.
func (db *Database) ListRunLogHistory(ctx context.Context, taskID uuid.UUID) ([]models.RunLogMeta, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, superseded_at FROM run_logs
		WHERE task_id = $1 ORDER BY superseded_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metas := []models.RunLogMeta{}
	for rows.Next() {
		var m models.RunLogMeta
		if err := rows.Scan(&m.ID, &m.SupersededAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// GetRunLogEntry fetches one superseded transcript by history id, scoped to
// the task so a caller authorized for one task can never read another task's
// history by guessing ids. Transparently inflates archived payloads, exactly
// like GetLog.
func (db *Database) GetRunLogEntry(ctx context.Context, taskID uuid.UUID, entryID int64) (*models.LogSession, error) {
	var sessionData *string
	var gz []byte
	var codec sql.NullString
	err := db.conn.QueryRowContext(ctx, `
		SELECT session_data, session_data_gz, session_compression
		FROM run_logs WHERE task_id = $1 AND id = $2`,
		taskID, entryID).Scan(&sessionData, &gz, &codec)
	if err != nil {
		return nil, err
	}
	raw, err := db.decodeLogRow(sessionData, gz, codec.String)
	if err != nil {
		return nil, err
	}
	var session models.LogSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// logScanChunk is the keyset page size for ForEachLog / GetAllLogs.
// One page of decoded sessions is the peak payload memory (#1122).
const logScanChunk = 64

// archiveScanChunk is the keyset page size for ArchiveOldLogs. Each
// candidate holds a live (uncompressed) payload, so the page is small
// on purpose — first-run archival of a year-old table stays bounded (#1122).
const archiveScanChunk = 32

// GetAllLogs gets all stored log sessions, transparently inflating archived
// payloads (#272). Implemented via ForEachLog so the scan itself is
// keyset-paginated; the returned map still holds every session (test /
// runner callers have small tables). Admin pipeline-metrics uses
// ForEachLog directly so it never materializes the full set (#1122).
func (db *Database) GetAllLogs(ctx context.Context) (map[uuid.UUID]*models.LogSession, error) {
	logs := make(map[uuid.UUID]*models.LogSession)
	err := db.ForEachLog(ctx, func(taskID uuid.UUID, session *models.LogSession) error {
		logs[taskID] = session
		return nil
	})
	return logs, err
}

// ForEachLog visits every stored log session in task_id order, inflating
// archived payloads. Sessions are fetched in keyset pages of logScanChunk
// so peak memory is one page + the callback's own working set (#1122).
// A decode/unmarshal failure skips that row (same as GetAllLogs).
func (db *Database) ForEachLog(ctx context.Context, fn func(uuid.UUID, *models.LogSession) error) error {
	var after uuid.UUID
	haveAfter := false
	for {
		n, last, err := db.scanLogPage(ctx, haveAfter, after, fn)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		after = last
		haveAfter = true
		if n < logScanChunk {
			return nil
		}
	}
}

func (db *Database) scanLogPage(ctx context.Context, haveAfter bool, after uuid.UUID, fn func(uuid.UUID, *models.LogSession) error) (int, uuid.UUID, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if haveAfter {
		rows, err = db.conn.QueryContext(ctx, `
			SELECT task_id, session_data, session_data_gz, session_compression
			FROM logs
			WHERE task_id > $1
			ORDER BY task_id
			LIMIT $2`, after, logScanChunk)
	} else {
		rows, err = db.conn.QueryContext(ctx, `
			SELECT task_id, session_data, session_data_gz, session_compression
			FROM logs
			ORDER BY task_id
			LIMIT $1`, logScanChunk)
	}
	if err != nil {
		return 0, uuid.Nil, err
	}
	defer rows.Close()

	n := 0
	var last uuid.UUID
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData *string
		var gz []byte
		var codec sql.NullString
		if err := rows.Scan(&taskID, &sessionData, &gz, &codec); err != nil {
			return n, last, err
		}
		last = taskID
		n++
		raw, err := db.decodeLogRow(sessionData, gz, codec.String)
		if err != nil {
			continue
		}
		var session models.LogSession
		if err := json.Unmarshal(raw, &session); err != nil {
			continue
		}
		if err := fn(taskID, &session); err != nil {
			return n, last, err
		}
	}
	return n, last, rows.Err()
}

// cleanupEligibleSubquery selects terminal task ids eligible for pruning (#252):
// older than the cutoff ($2) but NOT among the most recent $1 runs of their
// (prompt, recurrence) bucket — so the last-known state of any task is always
// kept regardless of age. Non-terminal tasks and rows with a NULL completed_at
// are never selected. Reused for the logs + tasks deletes within one tx; safe to
// run twice because the tasks ranking is unchanged between them (only logs are
// deleted first).
const cleanupEligibleSubquery = `
	SELECT id FROM (
		SELECT id, completed_at,
		       ROW_NUMBER() OVER (
		           PARTITION BY prompt, recurrence
		           ORDER BY completed_at DESC NULLS LAST
		       ) AS rn
		FROM tasks
		WHERE status IN ('success', 'error', 'cancelled')
	) ranked
	WHERE rn > $1 AND completed_at IS NOT NULL AND completed_at < $2`

// DeleteTask permanently removes one task and its transcripts, in a single
// transaction. See storage.DeleteTask for why deleting (not cancelling) is the
// only thing that frees a task's name.
//
// The two log tables are deleted explicitly because they hold a bare task_id
// with no foreign key (migrations 001 and 058); every other child table
// declares ON DELETE CASCADE. Ordered children-first so the task row is never
// orphaned mid-transaction.
func (db *Database) DeleteTask(ctx context.Context, taskID uuid.UUID) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	// A rollback after a successful Commit returns sql.ErrTxDone; the error
	// paths below already return the underlying failure.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM run_logs WHERE task_id = $1`, taskID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE task_id = $1`, taskID); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = $1`, taskID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected > 0, nil
}

// CleanupOldRuns prunes completed/error/cancelled task runs (and their logs)
// older than retentionDays, ALWAYS preserving the most recent keepPerTask runs
// per task bucket (prompt+recurrence) regardless of age (#252). retentionDays<=0
// disables pruning (returns 0) so a misconfiguration can never mass-delete.
// Returns the number of task rows deleted.
func (db *Database) CleanupOldRuns(ctx context.Context, retentionDays, keepPerTask int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	if keepPerTask < 0 {
		keepPerTask = 0
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM run_logs WHERE task_id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM logs WHERE task_id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM tasks WHERE id IN (`+cleanupEligibleSubquery+`)`,
		keepPerTask, cutoff)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}

// DeleteOldHistory deletes tasks and logs older than days, in one transaction.
func (db *Database) DeleteOldHistory(ctx context.Context, days int) (int, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM run_logs WHERE task_id IN (
			SELECT id FROM tasks
			WHERE status IN ($1, $2, $3) AND completed_at < $4
		)`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM logs WHERE task_id IN (
			SELECT id FROM tasks
			WHERE status IN ($1, $2, $3) AND completed_at < $4
		)`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
		DELETE FROM tasks
		WHERE status IN ($1, $2, $3) AND completed_at < $4`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}

// logArchiveCandidate is one live log payload eligible for archival.
type logArchiveCandidate struct {
	taskID uuid.UUID
	raw    []byte
}

// archiveCandidatesPage reads one keyset page of live (un-archived) log
// payloads for terminal tasks completed before cutoff. The cursor is fully
// drained and closed before return so the caller can UPDATE on the same
// (possibly single-conn) pool without deadlocking. after, when non-nil,
// is the exclusive lower bound on task_id (#1122).
func (db *Database) archiveCandidatesPage(ctx context.Context, cutoff time.Time, after *uuid.UUID, limit int) ([]logArchiveCandidate, error) {
	q := `
		SELECT l.task_id, l.session_data
		FROM logs l
		JOIN tasks t ON t.id = l.task_id
		WHERE t.status IN ($1, $2, $3)
		  AND t.completed_at < $4
		  AND l.session_data IS NOT NULL
		  AND l.session_compression IS NULL`
	args := []any{
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	}
	if after != nil {
		q += ` AND l.task_id > $5 ORDER BY l.task_id LIMIT $6`
		args = append(args, *after, limit)
	} else {
		q += ` ORDER BY l.task_id LIMIT $5`
		args = append(args, limit)
	}
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []logArchiveCandidate
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData string
		if err := rows.Scan(&taskID, &sessionData); err != nil {
			return nil, err
		}
		candidates = append(candidates, logArchiveCandidate{taskID: taskID, raw: []byte(sessionData)})
	}
	return candidates, rows.Err()
}

// ArchiveOldLogs compresses (and, when an archive key is configured, AES-256-GCM
// encrypts) the session_data payload of completed-task logs older than `days`,
// IN PLACE (#272): the payload moves into session_data_gz and session_data is
// nulled in a single per-row UPDATE. Only terminal tasks (success/error/
// cancelled) with a live payload are touched; already-archived rows
// (session_compression set) are skipped, so the sweep is idempotent. days<=0
// disables archival and returns (0, 0, nil) so a misconfiguration is inert.
//
// It returns the number of rows archived and the total bytes saved (the sum of
// raw-minus-stored sizes; ~always positive for real log payloads). Each row is
// committed independently: a row's archive write and its DB update are one
// statement, so there is no window where the payload exists in neither column.
// The per-row UPDATE inside the page loop is deliberate and is NOT an N+1 that
// batching would fix: every row carries a DIFFERENT payload compressed (and
// optionally encrypted) in Go, so there is nothing to join against, and folding
// a page of multi-megabyte blobs into one statement would trade the per-row
// commit boundary for a large memory spike.
// Candidates are fetched in keyset pages of archiveScanChunk so first-run
// archival of a large table stays memory-bounded (#1122).
func (db *Database) ArchiveOldLogs(ctx context.Context, days int) (int, int64, error) {
	if days <= 0 {
		return 0, 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	var archived int
	var bytesSaved int64
	var after *uuid.UUID
	for {
		candidates, err := db.archiveCandidatesPage(ctx, cutoff, after, archiveScanChunk)
		if err != nil {
			return archived, bytesSaved, err
		}
		if len(candidates) == 0 {
			return archived, bytesSaved, nil
		}
		for _, c := range candidates {
			stored, codec, err := encodeArchive(c.raw, db.archiveKey)
			if err != nil {
				return archived, bytesSaved, err
			}
			// One statement flips the row from live to archived: set the compressed
			// payload + codec and null session_data together. The guard re-checks
			// session_compression IS NULL so two concurrent sweeps can't double-archive.
			res, err := db.conn.ExecContext(ctx, `
				UPDATE logs
				SET session_data = NULL, session_data_gz = $1, session_compression = $2
				WHERE task_id = $3 AND session_compression IS NULL`,
				stored, codec, c.taskID)
			if err != nil {
				return archived, bytesSaved, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // raced by another sweep; leave counters untouched
			}
			archived++
			bytesSaved += int64(len(c.raw) - len(stored))
			id := c.taskID
			after = &id
		}
		if len(candidates) < archiveScanChunk {
			return archived, bytesSaved, nil
		}
	}
}

// Transaction support for atomic operations

// BeginTx starts a new transaction.
func (db *Database) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return db.conn.BeginTx(ctx, nil)
}

// GetTaskForUpdate gets a task by ID with a row-level lock. Must be in a tx.
func (db *Database) GetTaskForUpdate(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) (*models.Task, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = $1 FOR UPDATE", taskID)
	return db.scanTask(row)
}

// UpdateTaskTx updates a task within a transaction, via the registry-derived
// UPDATE statement (taskUpdateStatement, task_columns.go): id is the WHERE
// key, the txUpdate-flagged columns are SET in registry order. The columns
// deliberately absent (effective_priority, created_by_key_id, the wake/pause
// clocks, error_analysis, recurrence_spawned) are declared — with reasons —
// on their taskColumnRegistry rows. name, trigger_type, allow_event_triggers
// and serialization_key are in the set for import conflict=replace (#1104),
// the only tx write path that changes them: every other UpdateTaskTx caller
// writes back the values it scanned under the same row lock
// (GetTaskForUpdate / ClaimNextPendingTask), so for them these are no-op
// write-backs.
func (db *Database) UpdateTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	// Populate actual_duration_seconds (#274) on the same write that persists a
	// completed_at — mirrors AddTask so the storage call sites that go through
	// UpdateTaskTx (the terminal-status transitions) record the derived actual
	// without each one having to remember it.
	maybeComputeActualDuration(task)
	_, err := tx.ExecContext(ctx, taskUpdateStatement, updateTaskArgs(task)...)
	return err
}

// RecordSkip records a pre-run-gate skip on a still-scheduled task (#269): it
// re-locks the row, re-checks it is still `scheduled` (a concurrent cancel or
// claim must win and suppress the skip) AND still carries the scheduled_for
// the gate evaluation was dispatched against (a concurrent edit/reschedule
// must win too — see SettleGatedTask), advances scheduled_for to nextRun,
// increments skip_count, and stamps last_skip_at / last_skip_reason. status is
// intentionally left `scheduled` (no promotion to pending). Returns the task
// (updated when recorded, the fresh row as-is otherwise) and whether the skip
// was actually recorded. A nil observedScheduledFor skips the reschedule guard
// (status-only, the pre-async behavior); the scheduler always passes the
// fetched row's value.
func (db *Database) RecordSkip(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, reason string, nextRun time.Time, observedScheduledFor *time.Time) (*models.Task, bool, error) {
	task, err := db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, false, err
	}
	// Only a still-scheduled task can be skipped. A concurrent cancel/claim
	// (status moved off scheduled) wins and the skip is a no-op.
	if task.Status != models.TaskStatusScheduled {
		return task, false, nil
	}
	// A concurrent edit/reschedule moved scheduled_for while the gate ran: the
	// verdict is stale, so leave the operator's row untouched — the next due
	// tick re-evaluates the task's current definition.
	if observedScheduledFor != nil && !sameScheduledFor(task.ScheduledFor, observedScheduledFor) {
		return task, false, nil
	}
	now := time.Now().UTC()
	if !nextRun.IsZero() {
		task.ScheduledFor = &nextRun
	}
	task.SkipCount++
	task.LastSkipAt = &now
	if reason != "" {
		r := reason
		task.LastSkipReason = &r
	}
	if err := db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// sameScheduledFor reports whether a re-fetched row's scheduled_for still
// equals the one a gate evaluation was dispatched against. Compared at
// microsecond precision: timestamptz resolves to microseconds and the pgx
// encoders truncate a finer time.Time on write, so truncating both sides lets
// an in-memory value that never round-tripped through the DB match its stored
// twin.
func sameScheduledFor(current, observed *time.Time) bool {
	if current == nil || observed == nil {
		return current == nil && observed == nil
	}
	return current.Truncate(time.Microsecond).Equal(observed.Truncate(time.Microsecond))
}

// ── task_iterations (looped-task telemetry, #179) ──

const taskIterationColumns = "id, task_id, iteration_number, started_at, completed_at, worker_session_id, exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status"

// AddTaskIteration inserts or updates a per-iteration telemetry row (upsert on
// id, so a row created at iteration start can be finalized at iteration end).
func (db *Database) AddTaskIteration(ctx context.Context, it *models.TaskIteration) error {
	if it.ID == uuid.Nil {
		it.ID = uuid.New()
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO task_iterations (
			id, task_id, iteration_number, started_at, completed_at, worker_session_id,
			exit_condition_result, cost_usd, prompt_tokens, completion_tokens, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			completed_at = EXCLUDED.completed_at,
			worker_session_id = EXCLUDED.worker_session_id,
			exit_condition_result = EXCLUDED.exit_condition_result,
			cost_usd = EXCLUDED.cost_usd,
			prompt_tokens = EXCLUDED.prompt_tokens,
			completion_tokens = EXCLUDED.completion_tokens,
			status = EXCLUDED.status`,
		it.ID,
		it.TaskID,
		it.IterationNumber,
		it.StartedAt,
		it.CompletedAt,
		nullableString(it.WorkerSessionID),
		nullableString(it.ExitConditionResult),
		it.CostUSD,
		it.PromptTokens,
		it.CompletionTokens,
		it.Status,
	)
	return err
}

// ListTaskIterations returns a task's iterations in iteration_number order.
func (db *Database) ListTaskIterations(ctx context.Context, taskID uuid.UUID) ([]*models.TaskIteration, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskIterationColumns+" FROM task_iterations WHERE task_id = $1 ORDER BY iteration_number", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.TaskIteration
	for rows.Next() {
		it, serr := scanTaskIteration(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func scanTaskIteration(scanner interface{ Scan(...interface{}) error }) (*models.TaskIteration, error) {
	var (
		it                  models.TaskIteration
		completedAt         sql.NullTime
		workerSessionID     sql.NullString
		exitConditionResult sql.NullString
		costUSD             sql.NullFloat64
		promptTokens        sql.NullInt64
		completionTokens    sql.NullInt64
	)
	if err := scanner.Scan(
		&it.ID, &it.TaskID, &it.IterationNumber, &it.StartedAt, &completedAt,
		&workerSessionID, &exitConditionResult, &costUSD, &promptTokens, &completionTokens, &it.Status,
	); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t := completedAt.Time
		it.CompletedAt = &t
	}
	it.WorkerSessionID = workerSessionID.String
	it.ExitConditionResult = exitConditionResult.String
	it.CostUSD = costUSD.Float64
	it.PromptTokens = promptTokens.Int64
	it.CompletionTokens = completionTokens.Int64
	return &it, nil
}

// RecoverExpiredLeases resets tasks with expired leases back to pending. This is
// the crash-safe backstop: a worker that died mid-task (systemd restart) lets
// its lease expire, and recovery re-queues the task for the next claim.
//
// Recovery is attempt-bounded (#1116): a task whose attempt budget is spent is
// routed to the dead-letter queue instead of re-queued. Without the bound, a
// task that kills the process itself (reliably OOMs the binary, or crashes at
// every restart) cycled recover→claim→crash forever — the only max-retries
// check was the in-process failure path, which a crash never reaches. The
// predicate is attempt_count >= max_retries, EXACT parity with the in-process
// retry gate (runner: AttemptCount < MaxRetries requeues): max_retries=R
// allows at most R+1 total executions, and R=0 ("never retry") means exactly
// one — the crashed attempt was already the last allowed one, so recovery
// quarantines rather than granting a free extra run of the task's external
// side effects. (The issue text sketched a strict `>`; parity won in review.)
//
// The dead-letter branch mirrors DeadLetterTaskWithContext's column writes
// (status/completed_at/dead_lettered_at/dead_letter_reason/dead_letter_attempts/
// error_message, output_json nulled, lease cleared, actual_duration_seconds
// derived from started_at like maybeComputeActualDuration — NULL for a leased
// row that never started) so a recovery-quarantined row reads identically in
// the DLQ listing and is replayable the same way. It runs FIRST so a row never
// both dead-letters and re-queues; the two predicates are disjoint on
// attempt_count either way.
//
// Returns (requeued, deadLettered): the rows reset to pending, and the rows
// quarantined. The storage wrapper owns the telemetry for the latter.
func (db *Database) RecoverExpiredLeases(ctx context.Context, now time.Time) (int, int, error) {
	quarantined, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET
			status = $1,
			completed_at = $4,
			dead_lettered_at = $4,
			dead_letter_reason = $5,
			dead_letter_attempts = attempt_count + 1,
			error_message = $5,
			output_json = NULL,
			actual_duration_seconds = COALESCE(actual_duration_seconds, CASE
				WHEN started_at IS NOT NULL
				THEN GREATEST(0, EXTRACT(EPOCH FROM ($4::timestamptz - started_at)))::int
			END),
			lease_owner = NULL,
			lease_expires_at = NULL
		WHERE status IN ($2, $3)
		AND (lease_expires_at < $4 OR lease_expires_at IS NULL)
		AND attempt_count >= max_retries`,
		string(models.TaskStatusDeadLettered),
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		now,
		"crash-loop guard: the worker's lease expired past the retry budget (the process likely crashed or stalled mid-run on every attempt)",
	)
	if err != nil {
		return 0, 0, err
	}
	deadLettered, _ := quarantined.RowsAffected()

	result, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET
			status = $1,
			lease_owner = NULL,
			lease_expires_at = NULL,
			started_at = NULL,
			output_json = NULL,
			artifacts = NULL,
			attempt_count = attempt_count + 1
		WHERE status IN ($2, $3)
		AND (lease_expires_at < $4 OR lease_expires_at IS NULL)
		AND attempt_count < max_retries`,
		string(models.TaskStatusPending),
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		now,
	)
	if err != nil {
		return 0, int(deadLettered), err
	}
	affected, _ := result.RowsAffected()
	return int(affected), int(deadLettered), nil
}

// GetAllTasksPaginated gets tasks with pagination.
func (db *Database) GetAllTasksPaginated(ctx context.Context, limit, offset int) ([]*models.Task, int, error) {
	var total int
	err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+taskColumns+" FROM tasks ORDER BY created_at DESC LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := db.rowsToTasks(rows)
	return tasks, total, err
}

// TaskFilter contains optional filter parameters for task queries.
type TaskFilter struct {
	Status          *string
	Query           *string
	ScheduledOnly   bool
	CompletedToday  bool
	CompletedStatus *string
	CreatedBy       *uuid.UUID
	// HasDescription, when true, restricts to tasks carrying operator
	// documentation (#281): a non-null, non-empty description.
	HasDescription bool
	// Tags, when non-empty, restricts to tasks carrying ALL of these tags
	// (AND-semantics via jsonb containment) — #212.
	Tags []string
	// SourceTaskID, when set, restricts to tasks re-run/cloned from that source
	// task — the lineage view (#270).
	SourceTaskID *uuid.UUID
	// VisibleToUserID / VisibleToKeyID restrict to rows the principal created
	// (#1082): tasks whose created_by is this user, or whose created_by_key_id
	// is this API key. Set by the handler for principals without the
	// fleet-wide visibility grant; at most one is set per request. They AND
	// with every other filter, so a caller-supplied created_by can only
	// narrow further, never widen.
	VisibleToUserID *uuid.UUID
	VisibleToKeyID  *string
}

// GetTasksFiltered gets tasks with optional filters and pagination.
func (db *Database) GetTasksFiltered(ctx context.Context, filter TaskFilter, limit, offset int) ([]*models.Task, int, error) {
	whereClauses := []string{}
	args := []interface{}{}
	argIndex := 1

	if filter.Status != nil && *filter.Status != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIndex))
		args = append(args, *filter.Status)
		argIndex++
	}

	if filter.Query != nil && *filter.Query != "" {
		query := strings.TrimSpace(*filter.Query)
		if id, err := uuid.Parse(query); err == nil {
			whereClauses = append(whereClauses, fmt.Sprintf("id = $%d", argIndex))
			args = append(args, id)
			argIndex++
		} else {
			// title is matched alongside prompt: once a job has a title, the
			// title is what the operator remembers it by, and searching only the
			// prompt would fail to find a task by the label the list displays.
			whereClauses = append(whereClauses, fmt.Sprintf("(title ILIKE $%d OR prompt ILIKE $%d OR CAST(id AS TEXT) ILIKE $%d)", argIndex, argIndex, argIndex))
			args = append(args, "%"+query+"%")
			argIndex++
		}
	}

	if filter.ScheduledOnly {
		whereClauses = append(whereClauses, "(scheduled_for IS NOT NULL OR recurrence IS NOT NULL AND recurrence != '')")
	}

	if filter.CompletedToday {
		now := time.Now().UTC()
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)
		whereClauses = append(whereClauses, fmt.Sprintf("completed_at BETWEEN $%d AND $%d", argIndex, argIndex+1))
		args = append(args, todayStart, todayEnd)
		argIndex += 2
		if filter.CompletedStatus != nil && *filter.CompletedStatus != "" {
			whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIndex))
			args = append(args, *filter.CompletedStatus)
			argIndex++
		}
	}

	if filter.CreatedBy != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by = $%d", argIndex))
		args = append(args, *filter.CreatedBy)
		argIndex++
	}

	if filter.HasDescription {
		whereClauses = append(whereClauses, "description IS NOT NULL AND description <> ''")
	}

	if len(filter.Tags) > 0 {
		// AND-semantics: task tags must contain ALL requested tags. jsonb `@>`
		// (contains) over the GIN index; the bind value is a JSON array string.
		whereClauses = append(whereClauses, fmt.Sprintf("tags @> $%d::jsonb", argIndex))
		args = append(args, marshalTags(filter.Tags))
		argIndex++
	}

	if filter.SourceTaskID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("source_task_id = $%d", argIndex))
		args = append(args, filter.SourceTaskID.String())
		argIndex++
	}

	if filter.VisibleToUserID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by = $%d", argIndex))
		args = append(args, *filter.VisibleToUserID)
		argIndex++
	}

	if filter.VisibleToKeyID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_by_key_id = $%d", argIndex))
		args = append(args, *filter.VisibleToKeyID)
		argIndex++
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM tasks " + whereSQL
	err := db.conn.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	dataQuery := fmt.Sprintf("SELECT %s FROM tasks %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
		taskColumns, whereSQL, argIndex, argIndex+1)
	args = append(args, limit, offset)

	rows, err := db.conn.QueryContext(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	tasks, err := db.rowsToTasks(rows)
	return tasks, total, err
}

// TagCount is one row of the tag catalogue: a distinct tag and how many tasks
// carry it (#212).
type TagCount struct {
	Tag       string `json:"tag"`
	TaskCount int    `json:"task_count"`
}

// GetTagCatalogue returns every distinct tag in use with its task count, busiest
// first (then alphabetical). Drives GET /tasks/tags.
func (db *Database) GetTagCatalogue(ctx context.Context) ([]TagCount, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT tag, COUNT(*) AS task_count
		FROM tasks, jsonb_array_elements_text(tags) AS tag
		GROUP BY tag
		ORDER BY task_count DESC, tag ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.TaskCount); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// GetUsersByIDs gets users by a list of IDs efficiently.
func (db *Database) GetUsersByIDs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(userIDs) == 0 {
		return make(map[uuid.UUID]string), nil
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT id, username FROM users WHERE id = ANY($1::uuid[])", uuidStrings(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[uuid.UUID]string, len(userIDs))
	for rows.Next() {
		var id uuid.UUID
		var username string
		if err := rows.Scan(&id, &username); err != nil {
			return nil, err
		}
		result[id] = username
	}
	return result, rows.Err()
}
