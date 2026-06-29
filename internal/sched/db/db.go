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
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ErrDuplicateNodeName is returned by AddNode when a node with the same name
// already exists.
var ErrDuplicateNodeName = errors.New("node name already registered")

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
			id, username, password_hash, role, scopes, created_at, last_login, session_token, token_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			username = EXCLUDED.username,
			password_hash = EXCLUDED.password_hash,
			role = EXCLUDED.role,
			scopes = EXCLUDED.scopes,
			last_login = EXCLUDED.last_login,
			session_token = EXCLUDED.session_token,
			token_expires_at = EXCLUDED.token_expires_at`,
		user.ID,
		user.Username,
		user.PasswordHash,
		user.Role,
		marshalJSON(user.Scopes),
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

// CanNodeAccessFile checks if a node is assigned to an active task containing filename.
func (db *Database) CanNodeAccessFile(ctx context.Context, nodeID uuid.UUID, filename string) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tasks
			WHERE assigned_node_id = $1
			AND status IN ($2, $3)
			AND files ? $4
		)`,
		nodeID,
		string(models.TaskStatusRunning),
		string(models.TaskStatusAnalyzing),
		filename,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// GetUser gets a user by ID.
func (db *Database) GetUser(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, scopes, created_at, last_login, session_token, token_expires_at FROM users WHERE id = $1",
		userID)
	return db.rowToUser(row)
}

// GetUserByUsername gets a user by username.
func (db *Database) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
	row := db.conn.QueryRowContext(ctx,
		"SELECT id, username, password_hash, role, scopes, created_at, last_login, session_token, token_expires_at FROM users WHERE username = $1",
		username)
	return db.rowToUser(row)
}

// ListUsers returns all users ordered by username. Used by the admin CLI.
func (db *Database) ListUsers(ctx context.Context) ([]models.User, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT id, username, password_hash, role, scopes, created_at, last_login, session_token, token_expires_at FROM users ORDER BY username")
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
			scopes         string
			createdAt      time.Time
			lastLogin      sql.NullTime
			sessionToken   sql.NullString
			tokenExpiresAt sql.NullTime
		)
		if err := rows.Scan(&id, &username, &passwordHash, &role, &scopes, &createdAt, &lastLogin, &sessionToken, &tokenExpiresAt); err != nil {
			return nil, err
		}
		u := models.User{ID: id, Username: username, PasswordHash: passwordHash, Role: role, Scopes: unmarshalStringSlice(scopes), CreatedAt: createdAt}
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
		"SELECT id, username, password_hash, role, scopes, created_at, last_login, session_token, token_expires_at FROM users WHERE session_token = $1 AND (token_expires_at IS NULL OR token_expires_at > $2)",
		token, time.Now().UTC())
	return db.rowToUser(row)
}

func (db *Database) rowToUser(row *sql.Row) (*models.User, error) {
	var (
		id             uuid.UUID
		username       string
		passwordHash   string
		role           string
		scopes         string
		createdAt      time.Time
		lastLogin      sql.NullTime
		sessionToken   sql.NullString
		tokenExpiresAt sql.NullTime
	)

	err := row.Scan(&id, &username, &passwordHash, &role, &scopes, &createdAt, &lastLogin, &sessionToken, &tokenExpiresAt)
	if err != nil {
		return nil, err
	}

	user := &models.User{
		ID:           id,
		Username:     username,
		PasswordHash: passwordHash,
		Role:         role,
		Scopes:       unmarshalStringSlice(scopes),
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

// Node operations

// AddNode adds or updates a node in the registry.
func (db *Database) AddNode(ctx context.Context, node *models.Node) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO nodes (
			id, hostname, name, api_key, previous_api_key, key_rotated_at,
			os_type, status, last_heartbeat, current_task_id, registered_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			hostname = EXCLUDED.hostname,
			name = EXCLUDED.name,
			api_key = EXCLUDED.api_key,
			previous_api_key = EXCLUDED.previous_api_key,
			key_rotated_at = EXCLUDED.key_rotated_at,
			os_type = EXCLUDED.os_type,
			status = EXCLUDED.status,
			last_heartbeat = EXCLUDED.last_heartbeat,
			current_task_id = EXCLUDED.current_task_id,
			registered_at = EXCLUDED.registered_at`,
		node.ID,
		node.Hostname,
		node.Name,
		node.APIKey,
		node.PreviousAPIKey,
		node.KeyRotatedAt,
		node.OSType,
		string(node.Status),
		node.LastHeartbeat,
		node.CurrentTaskID,
		node.RegisteredAt,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_nodes_name_unique" {
		return ErrDuplicateNodeName
	}
	return err
}

func (db *Database) scanNode(scanner interface{ Scan(...interface{}) error }) (*models.Node, error) {
	var (
		id             uuid.UUID
		hostname       string
		name           string
		apiKey         string
		previousAPIKey sql.NullString
		keyRotatedAt   sql.NullTime
		osType         string
		status         string
		lastHeartbeat  time.Time
		currentTaskID  *uuid.UUID
		registeredAt   time.Time
	)

	err := scanner.Scan(&id, &hostname, &name, &apiKey, &previousAPIKey, &keyRotatedAt,
		&osType, &status, &lastHeartbeat, &currentTaskID, &registeredAt)
	if err != nil {
		return nil, err
	}

	node := &models.Node{
		ID:            id,
		Hostname:      hostname,
		Name:          name,
		APIKey:        apiKey,
		OSType:        osType,
		Status:        models.NodeStatus(status),
		LastHeartbeat: lastHeartbeat,
		CurrentTaskID: currentTaskID,
		RegisteredAt:  registeredAt,
	}
	if previousAPIKey.Valid {
		node.PreviousAPIKey = &previousAPIKey.String
	}
	if keyRotatedAt.Valid {
		node.KeyRotatedAt = &keyRotatedAt.Time
	}
	return node, nil
}

func (db *Database) rowToNode(row *sql.Row) (*models.Node, error) { return db.scanNode(row) }

func (db *Database) rowsToNodes(rows *sql.Rows) ([]*models.Node, error) {
	nodes := make([]*models.Node, 0)
	for rows.Next() {
		node, err := db.scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

const nodeColumns = "id, hostname, name, api_key, previous_api_key, key_rotated_at, os_type, status, last_heartbeat, current_task_id, registered_at"

// GetNode gets a node by ID.
func (db *Database) GetNode(ctx context.Context, nodeID uuid.UUID) (*models.Node, error) {
	row := db.conn.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = $1", nodeID)
	return db.rowToNode(row)
}

// GetNodeByAPIKey gets a node by its API key (also checks previous key in grace period).
func (db *Database) GetNodeByAPIKey(ctx context.Context, apiKey string) (*models.Node, error) {
	hashedKey := models.HashToken(apiKey)

	row := db.conn.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE api_key = $1", hashedKey)
	node, err := db.rowToNode(row)
	if err == nil {
		return node, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	row = db.conn.QueryRowContext(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		WHERE previous_api_key = $1
		AND key_rotated_at IS NOT NULL
		AND key_rotated_at > $2`,
		hashedKey,
		time.Now().UTC().Add(-models.KeyRotationGracePeriod),
	)
	return db.rowToNode(row)
}

// GetAllNodes gets all registered nodes.
func (db *Database) GetAllNodes(ctx context.Context) ([]*models.Node, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT "+nodeColumns+" FROM nodes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToNodes(rows)
}

// UpdateNode updates an existing node.
func (db *Database) UpdateNode(ctx context.Context, node *models.Node) error {
	return db.AddNode(ctx, node)
}

// RemoveNode removes a node from the registry.
func (db *Database) RemoveNode(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	result, err := db.conn.ExecContext(ctx, "DELETE FROM nodes WHERE id = $1", nodeID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

// Task operations

const taskColumns = "id, name, prompt, model, fallback_model, max_iterations, mcp_selection, priority, instruction_self_improve, status, assigned_node_id, agent_session_id, created_at, started_at, completed_at, result, error_message, scheduled_for, recurrence, created_by, files, lease_owner, lease_expires_at, attempt_count, max_retries, allow_network, timezone, created_by_key_id, trigger_type, credential_allowlist, loop_config, worktree_config, description, tags, retry_policy, source_task_id, persona, workspace_path, allow_task_creation, allow_recurring_task_creation, created_by_task_id, dead_lettered_at, dead_letter_reason, dead_letter_attempts, run_if, skip_count, last_skip_at, last_skip_reason, expected_duration_minutes, sla_warn_multiplier, sla_fail_multiplier, sla_breached, actual_duration_seconds"

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

// marshalTags serializes task tags for the JSONB column, ALWAYS as a JSON array
// (never the bare "null" marshalJSON emits for a nil slice) so the tags catalogue
// query's jsonb_array_elements_text never hits a scalar. Empty → "[]".
func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	return marshalJSON(tags)
}

// AddTask adds or updates a task.
func (db *Database) AddTask(ctx context.Context, task *models.Task) error {
	// Populate actual_duration_seconds (#274) whenever a completion timestamp
	// is present alongside a start. Done here (and in UpdateTaskTx) so EVERY
	// write path that persists a completed_at also persists the derived actual,
	// without each storage call site having to remember it. Idempotent: a
	// pre-set value (e.g. a test seed) is left untouched.
	maybeComputeActualDuration(task)
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO tasks (
			id, name, prompt, model, fallback_model, max_iterations, mcp_selection,
			priority, instruction_self_improve, status, assigned_node_id, agent_session_id,
			created_at, started_at, completed_at, result, error_message,
			scheduled_for, recurrence, created_by, files, lease_owner, lease_expires_at,
			attempt_count, max_retries, allow_network, timezone, created_by_key_id,
			trigger_type, credential_allowlist, loop_config, worktree_config, description, tags, retry_policy, source_task_id, persona, workspace_path,
			allow_task_creation, allow_recurring_task_creation, created_by_task_id,
			dead_lettered_at, dead_letter_reason, dead_letter_attempts,
			run_if, skip_count, last_skip_at, last_skip_reason,
			expected_duration_minutes, sla_warn_multiplier, sla_fail_multiplier,
			sla_breached, actual_duration_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			prompt = EXCLUDED.prompt,
			model = EXCLUDED.model,
			fallback_model = EXCLUDED.fallback_model,
			max_iterations = EXCLUDED.max_iterations,
			mcp_selection = EXCLUDED.mcp_selection,
			priority = EXCLUDED.priority,
			instruction_self_improve = EXCLUDED.instruction_self_improve,
			status = EXCLUDED.status,
			assigned_node_id = EXCLUDED.assigned_node_id,
			agent_session_id = EXCLUDED.agent_session_id,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			completed_at = EXCLUDED.completed_at,
			result = EXCLUDED.result,
			error_message = EXCLUDED.error_message,
			scheduled_for = EXCLUDED.scheduled_for,
			recurrence = EXCLUDED.recurrence,
			created_by = EXCLUDED.created_by,
			files = EXCLUDED.files,
			lease_owner = EXCLUDED.lease_owner,
			lease_expires_at = EXCLUDED.lease_expires_at,
			attempt_count = EXCLUDED.attempt_count,
			max_retries = EXCLUDED.max_retries,
			allow_network = EXCLUDED.allow_network,
			timezone = EXCLUDED.timezone,
			created_by_key_id = EXCLUDED.created_by_key_id,
			trigger_type = EXCLUDED.trigger_type,
			credential_allowlist = EXCLUDED.credential_allowlist,
			loop_config = EXCLUDED.loop_config,
			worktree_config = EXCLUDED.worktree_config,
			description = EXCLUDED.description,
			tags = EXCLUDED.tags,
			retry_policy = EXCLUDED.retry_policy,
			source_task_id = EXCLUDED.source_task_id,
			persona = EXCLUDED.persona,
			workspace_path = EXCLUDED.workspace_path,
			allow_task_creation = EXCLUDED.allow_task_creation,
			allow_recurring_task_creation = EXCLUDED.allow_recurring_task_creation,
			created_by_task_id = EXCLUDED.created_by_task_id,
			dead_lettered_at = EXCLUDED.dead_lettered_at,
			dead_letter_reason = EXCLUDED.dead_letter_reason,
			dead_letter_attempts = EXCLUDED.dead_letter_attempts,
			run_if = EXCLUDED.run_if,
			skip_count = EXCLUDED.skip_count,
			last_skip_at = EXCLUDED.last_skip_at,
			last_skip_reason = EXCLUDED.last_skip_reason,
			expected_duration_minutes = EXCLUDED.expected_duration_minutes,
			sla_warn_multiplier = EXCLUDED.sla_warn_multiplier,
			sla_fail_multiplier = EXCLUDED.sla_fail_multiplier,
			sla_breached = EXCLUDED.sla_breached,
			actual_duration_seconds = EXCLUDED.actual_duration_seconds`,
		task.ID,
		task.Name,
		task.Prompt,
		task.Model,
		task.FallbackModel,
		task.MaxIterations,
		marshalJSON(mcpSelectionOrEmpty(task.MCPSelection)),
		task.Priority,
		task.InstructionSelfImprove,
		string(task.Status),
		task.AssignedNodeID,
		task.AgentSessionID,
		task.CreatedAt,
		task.StartedAt,
		task.CompletedAt,
		task.Result,
		task.ErrorMessage,
		task.ScheduledFor,
		nullableString(task.Recurrence),
		task.CreatedBy,
		marshalJSON(task.Files),
		task.LeaseOwner,
		task.LeaseExpiresAt,
		task.AttemptCount,
		task.MaxRetries,
		task.AllowNetwork,
		taskTimezoneOrUTC(task.Timezone),
		task.CreatedByKeyID,
		triggerTypeOrCron(task.TriggerType),
		marshalCredentialAllowlist(task.CredentialAllowlist),
		marshalLoopConfig(task.LoopConfig),
		marshalWorktreeConfig(task.WorktreeConfig),
		nullableString(task.Description),
		marshalTags(task.Tags),
		marshalRetryPolicy(task.RetryPolicy),
		sourceTaskIDValue(task.SourceTaskID),
		nullableString(task.Persona),
		workspacePathValue(task.WorkspacePath),
		task.AllowTaskCreation,
		task.AllowRecurringTaskCreation,
		createdByTaskIDValue(task.CreatedByTaskID),
		task.DeadLetteredAt,
		nullableString(deref(task.DeadLetterReason)),
		deadLetterAttemptsValue(task.DeadLetterAttempts),
		marshalRunIf(task.RunIf),
		task.SkipCount,
		task.LastSkipAt,
		nullableString(deref(task.LastSkipReason)),
		expectedDurationValue(task.ExpectedDurationMinutes),
		slaMultiplierValue(task.SLAWarnMultiplier, models.DefaultSLAWarnMultiplier),
		slaMultiplierValue(task.SLAFailMultiplier, models.DefaultSLAFailMultiplier),
		task.SLABreached,
		task.ActualDurationSeconds,
	)
	return err
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

// taskInsertOnConflict is the static ON CONFLICT (id) DO UPDATE clause appended
// to every tasks INSERT. Kept in sync with AddTask's upsert. Extracted so the
// single-row, multi-row, and in-tx paths can never disagree.
const taskInsertOnConflict = ` ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			prompt = EXCLUDED.prompt,
			model = EXCLUDED.model,
			fallback_model = EXCLUDED.fallback_model,
			max_iterations = EXCLUDED.max_iterations,
			mcp_selection = EXCLUDED.mcp_selection,
			priority = EXCLUDED.priority,
			instruction_self_improve = EXCLUDED.instruction_self_improve,
			status = EXCLUDED.status,
			assigned_node_id = EXCLUDED.assigned_node_id,
			agent_session_id = EXCLUDED.agent_session_id,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			completed_at = EXCLUDED.completed_at,
			result = EXCLUDED.result,
			error_message = EXCLUDED.error_message,
			scheduled_for = EXCLUDED.scheduled_for,
			recurrence = EXCLUDED.recurrence,
			created_by = EXCLUDED.created_by,
			files = EXCLUDED.files,
			lease_owner = EXCLUDED.lease_owner,
			lease_expires_at = EXCLUDED.lease_expires_at,
			attempt_count = EXCLUDED.attempt_count,
			max_retries = EXCLUDED.max_retries,
			allow_network = EXCLUDED.allow_network,
			timezone = EXCLUDED.timezone,
			created_by_key_id = EXCLUDED.created_by_key_id,
			trigger_type = EXCLUDED.trigger_type,
			credential_allowlist = EXCLUDED.credential_allowlist,
			loop_config = EXCLUDED.loop_config,
			worktree_config = EXCLUDED.worktree_config,
			description = EXCLUDED.description,
			tags = EXCLUDED.tags,
			retry_policy = EXCLUDED.retry_policy,
			source_task_id = EXCLUDED.source_task_id,
			persona = EXCLUDED.persona,
			workspace_path = EXCLUDED.workspace_path,
			allow_task_creation = EXCLUDED.allow_task_creation,
			allow_recurring_task_creation = EXCLUDED.allow_recurring_task_creation,
			created_by_task_id = EXCLUDED.created_by_task_id,
			dead_lettered_at = EXCLUDED.dead_lettered_at,
			dead_letter_reason = EXCLUDED.dead_letter_reason,
			dead_letter_attempts = EXCLUDED.dead_letter_attempts,
			run_if = EXCLUDED.run_if,
			skip_count = EXCLUDED.skip_count,
			last_skip_at = EXCLUDED.last_skip_at,
			last_skip_reason = EXCLUDED.last_skip_reason,
			expected_duration_minutes = EXCLUDED.expected_duration_minutes,
			sla_warn_multiplier = EXCLUDED.sla_warn_multiplier,
			sla_fail_multiplier = EXCLUDED.sla_fail_multiplier,
			sla_breached = EXCLUDED.sla_breached,
			actual_duration_seconds = EXCLUDED.actual_duration_seconds`

// taskInsertColumns is the ordered column list for the tasks INSERT, kept in
// sync with AddTask / AddTaskBatch / AddTaskTx. Extracted as a constant so the
// single-row and multi-row builders never drift.
const taskInsertColumns = `id, name, prompt, model, fallback_model, max_iterations, mcp_selection,
			priority, instruction_self_improve, status, assigned_node_id, agent_session_id,
			created_at, started_at, completed_at, result, error_message,
			scheduled_for, recurrence, created_by, files, lease_owner, lease_expires_at,
			attempt_count, max_retries, allow_network, timezone, created_by_key_id,
			trigger_type, credential_allowlist, loop_config, worktree_config, description, tags, retry_policy, source_task_id, persona, workspace_path,
			allow_task_creation, allow_recurring_task_creation, created_by_task_id,
			dead_lettered_at, dead_letter_reason, dead_letter_attempts,
			run_if, skip_count, last_skip_at, last_skip_reason,
			expected_duration_minutes, sla_warn_multiplier, sla_fail_multiplier, sla_breached, actual_duration_seconds`

// taskInsertArgs returns the 53 positional INSERT values for a task, in the
// exact column order of taskInsertColumns. Shared by AddTask and AddTaskBatch so
// the single-row and multi-row paths can never disagree on argument ordering.
// It derives actual_duration_seconds (#274) up front so the batch/tx paths
// persist it identically to the single-row AddTask path.
func taskInsertArgs(t *models.Task) []any {
	maybeComputeActualDuration(t)
	return []any{
		t.ID,
		t.Name,
		t.Prompt,
		t.Model,
		t.FallbackModel,
		t.MaxIterations,
		marshalJSON(mcpSelectionOrEmpty(t.MCPSelection)),
		t.Priority,
		t.InstructionSelfImprove,
		string(t.Status),
		t.AssignedNodeID,
		t.AgentSessionID,
		t.CreatedAt,
		t.StartedAt,
		t.CompletedAt,
		t.Result,
		t.ErrorMessage,
		t.ScheduledFor,
		nullableString(t.Recurrence),
		t.CreatedBy,
		marshalJSON(t.Files),
		t.LeaseOwner,
		t.LeaseExpiresAt,
		t.AttemptCount,
		t.MaxRetries,
		t.AllowNetwork,
		taskTimezoneOrUTC(t.Timezone),
		t.CreatedByKeyID,
		triggerTypeOrCron(t.TriggerType),
		marshalCredentialAllowlist(t.CredentialAllowlist),
		marshalLoopConfig(t.LoopConfig),
		marshalWorktreeConfig(t.WorktreeConfig),
		nullableString(t.Description),
		marshalTags(t.Tags),
		marshalRetryPolicy(t.RetryPolicy),
		sourceTaskIDValue(t.SourceTaskID),
		nullableString(t.Persona),
		workspacePathValue(t.WorkspacePath),
		t.AllowTaskCreation,
		t.AllowRecurringTaskCreation,
		createdByTaskIDValue(t.CreatedByTaskID),
		t.DeadLetteredAt,
		nullableString(deref(t.DeadLetterReason)),
		deadLetterAttemptsValue(t.DeadLetterAttempts),
		marshalRunIf(t.RunIf),
		t.SkipCount,
		t.LastSkipAt,
		nullableString(deref(t.LastSkipReason)),
		expectedDurationValue(t.ExpectedDurationMinutes),
		slaMultiplierValue(t.SLAWarnMultiplier, models.DefaultSLAWarnMultiplier),
		slaMultiplierValue(t.SLAFailMultiplier, models.DefaultSLAFailMultiplier),
		t.SLABreached,
		t.ActualDurationSeconds,
	}
}

// taskInsertColumnsCount is the number of columns in taskInsertColumns. Kept as
// a named const so the multi-row placeholder builder is self-documenting and
// a future schema migration that adds a column forces a single touch point.
const taskInsertColumnsCount = 53

// AddTaskBatch inserts a slice of tasks in a single parameterised INSERT (#227),
// replacing N sequential ExecContext round-trips. It does NOT run inside an
// explicit transaction — callers that need atomicity wrap the call in BeginTx /
// Commit (see Storage.AddTaskBatch). An empty slice is a no-op.
//
// Each row carries the SAME 53 columns as AddTask (via the shared
// taskInsertArgs helper), so a row inserted through the batch path is
// byte-identical to one inserted through the single-row path.
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

	const cols = taskInsertColumnsCount
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
func (db *Database) AddTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	args := taskInsertArgs(task)
	var q strings.Builder
	q.WriteString("INSERT INTO tasks (")
	q.WriteString(taskInsertColumns)
	q.WriteString(") VALUES ($1")
	for i := 2; i <= taskInsertColumnsCount; i++ {
		fmt.Fprintf(&q, ",$%d", i)
	}
	q.WriteByte(')')
	q.WriteString(taskInsertOnConflict)
	_, err := tx.ExecContext(ctx, q.String(), args...)
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

func (db *Database) scanTask(scanner interface{ Scan(...interface{}) error }) (*models.Task, error) {
	var (
		id                     uuid.UUID
		name                   string
		prompt                 string
		model                  sql.NullString
		fallbackModel          sql.NullString
		maxIterations          sql.NullInt64
		mcpSelection           sql.NullString
		priority               int
		instructionSelfImprove bool
		status                 string
		assignedNodeID         *uuid.UUID
		agentSessionID         sql.NullString
		createdAt              time.Time
		startedAt              sql.NullTime
		completedAt            sql.NullTime
		result                 sql.NullString
		errorMessage           sql.NullString
		scheduledFor           sql.NullTime
		recurrence             sql.NullString
		createdBy              *uuid.UUID
		files                  sql.NullString
		leaseOwner             sql.NullString
		leaseExpiresAt         sql.NullTime
		attemptCount           int
		maxRetries             int
		allowNetwork           bool
		timezone               sql.NullString
		createdByKeyID         sql.NullString
		triggerType            sql.NullString
		credentialAllowlist    sql.NullString
		loopConfig             sql.NullString
		worktreeConfig         sql.NullString
		description            sql.NullString
		tags                   sql.NullString
		retryPolicy            sql.NullString
		sourceTaskID           sql.NullString
		persona                sql.NullString
		workspacePath          sql.NullString
		allowTaskCreation      bool
		allowRecurringTaskCre  bool
		createdByTaskID        sql.NullString
		deadLetteredAt         sql.NullTime
		deadLetterReason       sql.NullString
		deadLetterAttempts     sql.NullInt64
		runIf                  sql.NullString
		skipCount              int
		lastSkipAt             sql.NullTime
		lastSkipReason         sql.NullString
		expectedDur            sql.NullInt64
		slaWarnMul             sql.NullFloat64
		slaFailMul             sql.NullFloat64
		slaBreached            bool
		actualDurSecs          sql.NullInt64
	)

	err := scanner.Scan(
		&id, &name, &prompt, &model, &fallbackModel, &maxIterations, &mcpSelection,
		&priority, &instructionSelfImprove, &status, &assignedNodeID, &agentSessionID,
		&createdAt, &startedAt, &completedAt, &result, &errorMessage,
		&scheduledFor, &recurrence, &createdBy, &files, &leaseOwner, &leaseExpiresAt,
		&attemptCount, &maxRetries, &allowNetwork, &timezone, &createdByKeyID,
		&triggerType, &credentialAllowlist, &loopConfig, &worktreeConfig, &description, &tags, &retryPolicy, &sourceTaskID, &persona, &workspacePath,
		&allowTaskCreation, &allowRecurringTaskCre, &createdByTaskID,
		&deadLetteredAt, &deadLetterReason, &deadLetterAttempts,
		&runIf, &skipCount, &lastSkipAt, &lastSkipReason,
		&expectedDur, &slaWarnMul, &slaFailMul, &slaBreached, &actualDurSecs,
	)
	if err != nil {
		return nil, err
	}

	task := &models.Task{
		ID:                     id,
		Name:                   name,
		Prompt:                 prompt,
		Priority:               priority,
		InstructionSelfImprove: instructionSelfImprove,
		Status:                 models.TaskStatus(status),
		AssignedNodeID:         assignedNodeID,
		CreatedAt:              createdAt,
		CreatedBy:              createdBy,
		AttemptCount:           attemptCount,
		MaxRetries:             maxRetries,
		AllowNetwork:           allowNetwork,
		Timezone:               taskTimezoneOrUTC(timezone.String),
		TriggerType:            models.TriggerType(triggerTypeOrCronStr(triggerType.String)),
	}
	if model.Valid {
		task.Model = &model.String
	}
	if fallbackModel.Valid {
		task.FallbackModel = &fallbackModel.String
	}
	if maxIterations.Valid {
		value := int(maxIterations.Int64)
		task.MaxIterations = &value
	}
	if mcpSelection.Valid {
		task.MCPSelection = unmarshalMCPSelection(mcpSelection.String)
	} else {
		task.MCPSelection = models.MCPSelection{}
	}
	// NULL → nil (inherit global); "[]" → non-nil empty (deny all). The
	// distinction is load-bearing for Gate-3, so do NOT coerce nil to empty.
	task.CredentialAllowlist = unmarshalCredentialAllowlist(credentialAllowlist)
	task.LoopConfig = unmarshalLoopConfig(loopConfig)
	task.WorktreeConfig = unmarshalWorktreeConfig(worktreeConfig)
	task.RetryPolicy = unmarshalRetryPolicy(retryPolicy)
	task.Persona = persona.String
	if sourceTaskID.Valid && sourceTaskID.String != "" {
		if sid, perr := uuid.Parse(sourceTaskID.String); perr == nil {
			task.SourceTaskID = &sid
		} else {
			log.Printf("Warning: invalid source_task_id %q: %v", sourceTaskID.String, perr)
		}
	}
	task.AllowTaskCreation = allowTaskCreation
	task.AllowRecurringTaskCreation = allowRecurringTaskCre
	if createdByTaskID.Valid && createdByTaskID.String != "" {
		if cid, perr := uuid.Parse(createdByTaskID.String); perr == nil {
			task.CreatedByTaskID = &cid
		} else {
			log.Printf("Warning: invalid created_by_task_id %q: %v", createdByTaskID.String, perr)
		}
	}
	task.Description = description.String
	if agentSessionID.Valid {
		task.AgentSessionID = &agentSessionID.String
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		task.CompletedAt = &completedAt.Time
	}
	if result.Valid {
		task.Result = &result.String
	}
	if errorMessage.Valid {
		task.ErrorMessage = &errorMessage.String
	}
	if scheduledFor.Valid {
		task.ScheduledFor = &scheduledFor.Time
	}
	if recurrence.Valid {
		task.Recurrence = recurrence.String
	}
	if files.Valid {
		task.Files = unmarshalStringSlice(files.String)
	}
	// tags is NOT NULL DEFAULT '[]', so it's always present; assign independently
	// of files (unmarshalStringSlice maps ""/"null" → empty slice safely).
	task.Tags = unmarshalStringSlice(tags.String)
	if leaseOwner.Valid {
		task.LeaseOwner = &leaseOwner.String
	}
	if leaseExpiresAt.Valid {
		task.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if createdByKeyID.Valid {
		task.CreatedByKeyID = &createdByKeyID.String
	}
	if workspacePath.Valid && workspacePath.String != "" {
		task.WorkspacePath = &workspacePath.String
	}
	if deadLetteredAt.Valid {
		task.DeadLetteredAt = &deadLetteredAt.Time
	}
	if deadLetterReason.Valid {
		task.DeadLetterReason = &deadLetterReason.String
	}
	if deadLetterAttempts.Valid {
		task.DeadLetterAttempts = int(deadLetterAttempts.Int64)
	}
	task.RunIf = unmarshalRunIf(runIf)
	task.SkipCount = skipCount
	if lastSkipAt.Valid {
		task.LastSkipAt = &lastSkipAt.Time
	}
	if lastSkipReason.Valid {
		task.LastSkipReason = &lastSkipReason.String
	}
	// SLA columns (#274). expected_duration_minutes / actual_duration_seconds
	// are NULLable (NULL = no SLA / not-yet-terminal); the multipliers are NOT
	// NULL DEFAULT so they are always present — normalize a stray zero to the
	// default so a downstream monitor / report never divides by zero.
	if expectedDur.Valid {
		v := int(expectedDur.Int64)
		task.ExpectedDurationMinutes = &v
	}
	task.SLAWarnMultiplier = slaMultiplierValue(slaWarnMul.Float64, models.DefaultSLAWarnMultiplier)
	task.SLAFailMultiplier = slaMultiplierValue(slaFailMul.Float64, models.DefaultSLAFailMultiplier)
	task.SLABreached = slaBreached
	if actualDurSecs.Valid {
		v := int(actualDurSecs.Int64)
		task.ActualDurationSeconds = &v
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

// GetAllTasks gets all tasks.
func (db *Database) GetAllTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
}

// GetScheduledTasks gets scheduled tasks that are due to run up to a limit.
func (db *Database) GetScheduledTasks(ctx context.Context, cutoff time.Time, limit int) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		AND scheduled_for IS NOT NULL
		AND scheduled_for <= $2
		AND trigger_type = 'cron'
		ORDER BY scheduled_for ASC
		LIMIT $3`,
		string(models.TaskStatusScheduled), cutoff, limit)
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

// UpdateTasksModelBatch updates model + fallback_model of scheduled tasks.
func (db *Database) UpdateTasksModelBatch(ctx context.Context, model, fallbackModel, fromModel string) (int, error) {
	var res sql.Result
	var err error
	// fallback_model is nullable TEXT: an empty fallback must persist as NULL (not
	// ""), matching the per-task nullableString path so scanTask reads it back as a
	// nil *string. model stays raw — callers (handler + CLI) require it non-empty.
	if fromModel != "" {
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3 AND model = $4`,
			model, nullableString(fallbackModel), string(models.TaskStatusScheduled), fromModel)
	} else {
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3`,
			model, nullableString(fallbackModel), string(models.TaskStatusScheduled))
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

// GetPendingTasks gets all pending tasks, sorted by priority DESC, created_at ASC.
func (db *Database) GetPendingTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status = $1
		ORDER BY priority DESC, created_at ASC`,
		string(models.TaskStatusPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToTasks(rows)
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
		ORDER BY priority DESC, created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
		string(models.TaskStatusPending))
	task, err := db.scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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

// GetRunningTasks gets all currently running tasks.
func (db *Database) GetRunningTasks(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2, $3)`,
		string(models.TaskStatusRunning),
		string(models.TaskStatusAnalyzing),
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
// "In-flight" mirrors GetRunningTasks: leased / running / analyzing — the
// statuses where StartedAt is set and the task has not yet reached a terminal
// state. The partial index idx_tasks_sla does NOT cover this query (it is
// keyed on completed_at), but the in-flight set is small (one host, capped
// pool) so a seq scan filtered by status + expected_duration_minutes IS NOT
// NULL is cheap; an extra index would not pay for itself.
func (db *Database) GetRunningTasksWithSLA(ctx context.Context) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE status IN ($1, $2, $3)
		AND expected_duration_minutes IS NOT NULL`,
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		string(models.TaskStatusAnalyzing))
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

// GetSLAReport aggregates the per-prompt SLA actuals over the last windowDays
// (#274): the p50/p95 actual run duration and the breach rate for each
// (prompt, expected_duration_minutes) bucket. Rows without an expected duration
// or an actual duration are excluded. windowDays is clamped to [1, 90]; the
// partial index idx_tasks_sla backs the WHERE filter. Buckets are ordered by
// breach rate (worst first) so the most violated SLAs surface at the top.
func (db *Database) GetSLAReport(ctx context.Context, windowDays int) (*models.SLAReport, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	if windowDays > 90 {
		windowDays = 90
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			prompt                                                  AS task_name,
			expected_duration_minutes,
			COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY actual_duration_seconds), 0) / 60.0,
			CASE WHEN COUNT(*) = 0 THEN 0.0
			     ELSE 100.0 * SUM(CASE WHEN sla_breached THEN 1 ELSE 0 END) / COUNT(*) END,
			COUNT(*)
		FROM tasks
		WHERE completed_at >= NOW() - ($1 || ' days')::INTERVAL
		AND expected_duration_minutes IS NOT NULL
		AND actual_duration_seconds IS NOT NULL
		GROUP BY prompt, expected_duration_minutes
		ORDER BY 5 DESC, prompt ASC`,
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

	err := db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total_nodes,
			COUNT(*) FILTER (WHERE status IN ($1, $2)) as active_nodes,
			COUNT(*) FILTER (WHERE status = $1) as idle_nodes,
			COUNT(*) FILTER (WHERE status = $3) as offline_nodes
		FROM nodes`,
		string(models.NodeStatusIdle),
		string(models.NodeStatusBusy),
		string(models.NodeStatusOffline),
	).Scan(&stats.TotalNodes, &stats.ActiveNodes, &stats.IdleNodes, &stats.OfflineNodes)
	if err != nil {
		return nil, fmt.Errorf("failed to get node stats: %w", err)
	}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	err = db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = $1) as pending_tasks,
			COUNT(*) FILTER (WHERE status IN ($2, $3, $8)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $4 AND completed_at BETWEEN $5 AND $6) as completed_today,
			COUNT(*) FILTER (WHERE status = $7 AND completed_at BETWEEN $5 AND $6) as failed_today
		FROM tasks`,
		string(models.TaskStatusPending),
		string(models.TaskStatusRunning),
		string(models.TaskStatusAnalyzing),
		string(models.TaskStatusSuccess),
		todayStart,
		todayEnd,
		string(models.TaskStatusError),
		string(models.TaskStatusLeased),
	).Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasksToday, &stats.FailedTasksToday)
	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}
	return stats, nil
}

// GetDashboardStatsForUser gets stats scoped to a user's permissions. Scopes are
// glob patterns matched against node names. Tasks are visible when created by
// the user or untargeted (mcp_selection-based tasks carry no node target, so all
// non-creator tasks fall under the untargeted branch).
func (db *Database) GetDashboardStatsForUser(ctx context.Context, userID *uuid.UUID, scopes []string) (*models.DashboardStats, error) {
	if len(scopes) == 0 {
		return db.GetDashboardStats(ctx)
	}

	stats := &models.DashboardStats{}

	likePatterns := make([]string, len(scopes))
	for i, scope := range scopes {
		likePatterns[i] = globToLike(scope)
	}

	err := db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total_nodes,
			COUNT(*) FILTER (WHERE status IN ($1, $2)) as active_nodes,
			COUNT(*) FILTER (WHERE status = $1) as idle_nodes,
			COUNT(*) FILTER (WHERE status = $3) as offline_nodes
		FROM nodes
		WHERE name LIKE ANY($4::text[])`,
		string(models.NodeStatusIdle),
		string(models.NodeStatusBusy),
		string(models.NodeStatusOffline),
		likePatterns,
	).Scan(&stats.TotalNodes, &stats.ActiveNodes, &stats.IdleNodes, &stats.OfflineNodes)
	if err != nil {
		return nil, fmt.Errorf("failed to get node stats: %w", err)
	}

	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, time.UTC)

	// Without node targeting, a scoped user sees their own tasks plus all
	// untargeted tasks (every task is untargeted now).
	var args []interface{}
	args = append(args,
		string(models.TaskStatusPending),   // $1
		string(models.TaskStatusRunning),   // $2
		string(models.TaskStatusAnalyzing), // $3
		string(models.TaskStatusSuccess),   // $4
		todayStart,                         // $5
		todayEnd,                           // $6
		string(models.TaskStatusError),     // $7
		string(models.TaskStatusLeased),    // $8
	)
	whereClause := "TRUE" // all tasks are untargeted → visible to any scoped user
	if userID != nil {
		args = append(args, *userID) // $9
		whereClause = fmt.Sprintf("(created_by = $%d OR TRUE)", len(args))
	}

	query := fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE status = $1) as pending_tasks,
			COUNT(*) FILTER (WHERE status IN ($2, $3, $8)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $4 AND completed_at BETWEEN $5 AND $6) as completed_today,
			COUNT(*) FILTER (WHERE status = $7 AND completed_at BETWEEN $5 AND $6) as failed_today
		FROM tasks
		WHERE %s`, whereClause)

	err = db.conn.QueryRowContext(ctx, query, args...).Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasksToday, &stats.FailedTasksToday)
	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}
	return stats, nil
}

// Log operations

// AddLog stores a log session for a task. The payload is always written live
// (plaintext JSON in session_data); the archival columns are reset so a re-write
// of a previously archived row returns it to the live, uncompressed state.
func (db *Database) AddLog(ctx context.Context, taskID uuid.UUID, session *models.LogSession) error {
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO logs (task_id, session_data, session_data_gz, session_compression)
		VALUES ($1, $2, NULL, NULL)
		ON CONFLICT (task_id) DO UPDATE SET
			session_data = EXCLUDED.session_data,
			session_data_gz = NULL,
			session_compression = NULL`,
		taskID, string(sessionJSON))
	return err
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

// GetAllLogs gets all stored log sessions, transparently inflating archived
// payloads (#272).
func (db *Database) GetAllLogs(ctx context.Context) (map[uuid.UUID]*models.LogSession, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT task_id, session_data, session_data_gz, session_compression FROM logs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make(map[uuid.UUID]*models.LogSession)
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData *string
		var gz []byte
		var codec sql.NullString
		if err := rows.Scan(&taskID, &sessionData, &gz, &codec); err != nil {
			return nil, err
		}
		raw, err := db.decodeLogRow(sessionData, gz, codec.String)
		if err != nil {
			continue
		}
		var session models.LogSession
		if err := json.Unmarshal(raw, &session); err != nil {
			continue
		}
		logs[taskID] = &session
	}
	return logs, rows.Err()
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

// archiveCandidates reads the live (un-archived) log payloads of terminal tasks
// completed before cutoff, fully draining and closing the cursor before it
// returns so the caller can issue UPDATEs on the same (possibly single-conn)
// pool without deadlocking.
func (db *Database) archiveCandidates(ctx context.Context, cutoff time.Time) ([]logArchiveCandidate, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT l.task_id, l.session_data
		FROM logs l
		JOIN tasks t ON t.id = l.task_id
		WHERE t.status IN ($1, $2, $3)
		  AND t.completed_at < $4
		  AND l.session_data IS NOT NULL
		  AND l.session_compression IS NULL`,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusCancelled),
		cutoff,
	)
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
func (db *Database) ArchiveOldLogs(ctx context.Context, days int) (int, int64, error) {
	if days <= 0 {
		return 0, 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	candidates, err := db.archiveCandidates(ctx, cutoff)
	if err != nil {
		return 0, 0, err
	}

	var archived int
	var bytesSaved int64
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
	}
	return archived, bytesSaved, nil
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

// UpdateTaskTx updates a task within a transaction.
func (db *Database) UpdateTaskTx(ctx context.Context, tx *sql.Tx, task *models.Task) error {
	// Populate actual_duration_seconds (#274) on the same write that persists a
	// completed_at — mirrors AddTask so the storage call sites that go through
	// UpdateTaskTx (the terminal-status transitions) record the derived actual
	// without each one having to remember it.
	maybeComputeActualDuration(task)
	_, err := tx.ExecContext(ctx, `
		UPDATE tasks SET
			prompt = $2,
			mcp_selection = $3,
			priority = $4,
			instruction_self_improve = $5,
			status = $6,
			assigned_node_id = $7,
			agent_session_id = $8,
			created_at = $9,
			started_at = $10,
			completed_at = $11,
			result = $12,
			error_message = $13,
			scheduled_for = $14,
			recurrence = $15,
			created_by = $16,
			files = $17,
			lease_owner = $18,
			lease_expires_at = $19,
			model = $20,
			fallback_model = $21,
			max_iterations = $22,
			attempt_count = $23,
			max_retries = $24,
			allow_network = $25,
			timezone = $26,
			credential_allowlist = $27,
			loop_config = $28,
			worktree_config = $29,
			description = $30,
			tags = $31,
			retry_policy = $32,
			source_task_id = $33,
			persona = $34,
			workspace_path = $35,
			allow_task_creation = $36,
			allow_recurring_task_creation = $37,
			created_by_task_id = $38,
			dead_lettered_at = $39,
			dead_letter_reason = $40,
			dead_letter_attempts = $41,
			run_if = $42,
			skip_count = $43,
			last_skip_at = $44,
			last_skip_reason = $45,
			expected_duration_minutes = $46,
			sla_warn_multiplier = $47,
			sla_fail_multiplier = $48,
			sla_breached = $49,
			actual_duration_seconds = $50
		WHERE id = $1`,
		task.ID,
		task.Prompt,
		marshalJSON(mcpSelectionOrEmpty(task.MCPSelection)),
		task.Priority,
		task.InstructionSelfImprove,
		string(task.Status),
		task.AssignedNodeID,
		task.AgentSessionID,
		task.CreatedAt,
		task.StartedAt,
		task.CompletedAt,
		task.Result,
		task.ErrorMessage,
		task.ScheduledFor,
		nullableString(task.Recurrence),
		task.CreatedBy,
		marshalJSON(task.Files),
		task.LeaseOwner,
		task.LeaseExpiresAt,
		task.Model,
		task.FallbackModel,
		task.MaxIterations,
		task.AttemptCount,
		task.MaxRetries,
		task.AllowNetwork,
		taskTimezoneOrUTC(task.Timezone),
		marshalCredentialAllowlist(task.CredentialAllowlist),
		marshalLoopConfig(task.LoopConfig),
		marshalWorktreeConfig(task.WorktreeConfig),
		nullableString(task.Description),
		marshalTags(task.Tags),
		marshalRetryPolicy(task.RetryPolicy),
		sourceTaskIDValue(task.SourceTaskID),
		nullableString(task.Persona),
		workspacePathValue(task.WorkspacePath),
		task.AllowTaskCreation,
		task.AllowRecurringTaskCreation,
		createdByTaskIDValue(task.CreatedByTaskID),
		task.DeadLetteredAt,
		nullableString(deref(task.DeadLetterReason)),
		deadLetterAttemptsValue(task.DeadLetterAttempts),
		marshalRunIf(task.RunIf),
		task.SkipCount,
		task.LastSkipAt,
		nullableString(deref(task.LastSkipReason)),
		expectedDurationValue(task.ExpectedDurationMinutes),
		slaMultiplierValue(task.SLAWarnMultiplier, models.DefaultSLAWarnMultiplier),
		slaMultiplierValue(task.SLAFailMultiplier, models.DefaultSLAFailMultiplier),
		task.SLABreached,
		task.ActualDurationSeconds,
	)
	return err
}

// RecordSkip records a pre-run-gate skip on a still-scheduled task (#269): it
// re-locks the row, re-checks it is still `scheduled` (a concurrent cancel or
// claim must win and suppress the skip), advances scheduled_for to nextRun,
// increments skip_count, and stamps last_skip_at / last_skip_reason. status is
// intentionally left `scheduled` (no promotion to pending). Returns the updated
// task.
func (db *Database) RecordSkip(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, reason string, nextRun time.Time) (*models.Task, error) {
	task, err := db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	// Only a still-scheduled task can be skipped. A concurrent cancel/claim
	// (status moved off scheduled) wins and the skip is a no-op.
	if task.Status != models.TaskStatusScheduled {
		return task, nil
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
		return nil, err
	}
	return task, nil
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

// GetNodeForUpdate gets a node by ID with a row-level lock. Must be in a tx.
func (db *Database) GetNodeForUpdate(ctx context.Context, tx *sql.Tx, nodeID uuid.UUID) (*models.Node, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE id = $1 FOR UPDATE", nodeID)
	return db.scanNode(row)
}

// UpdateNodeTx updates a node within a transaction.
func (db *Database) UpdateNodeTx(ctx context.Context, tx *sql.Tx, node *models.Node) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE nodes SET
			hostname = $2,
			name = $3,
			api_key = $4,
			previous_api_key = $5,
			key_rotated_at = $6,
			os_type = $7,
			status = $8,
			last_heartbeat = $9,
			current_task_id = $10,
			registered_at = $11
		WHERE id = $1`,
		node.ID,
		node.Hostname,
		node.Name,
		node.APIKey,
		node.PreviousAPIKey,
		node.KeyRotatedAt,
		node.OSType,
		string(node.Status),
		node.LastHeartbeat,
		node.CurrentTaskID,
		node.RegisteredAt,
	)
	return err
}

// RecoverExpiredLeases resets tasks with expired leases back to pending. This is
// the crash-safe backstop: a worker that died mid-task (systemd restart) lets
// its lease expire, and recovery re-queues the task for the next claim.
func (db *Database) RecoverExpiredLeases(ctx context.Context, now time.Time) (int, error) {
	result, err := db.conn.ExecContext(ctx, `
		UPDATE tasks SET
			status = $1,
			assigned_node_id = NULL,
			lease_owner = NULL,
			lease_expires_at = NULL,
			started_at = NULL,
			attempt_count = attempt_count + 1
		WHERE status IN ($2, $3, $4)
		AND (lease_expires_at < $5 OR lease_expires_at IS NULL)`,
		string(models.TaskStatusPending),
		string(models.TaskStatusLeased),
		string(models.TaskStatusRunning),
		string(models.TaskStatusAnalyzing),
		now,
	)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// GetNodeByName gets a node by its name directly.
func (db *Database) GetNodeByName(ctx context.Context, name string) (*models.Node, error) {
	row := db.conn.QueryRowContext(ctx, "SELECT "+nodeColumns+" FROM nodes WHERE name = $1", name)
	node, err := db.scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return node, err
}

// GetNodeNamesByIDs gets node names for a list of node IDs efficiently.
func (db *Database) GetNodeNamesByIDs(ctx context.Context, nodeIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(nodeIDs) == 0 {
		return make(map[uuid.UUID]string), nil
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT id, name FROM nodes WHERE id = ANY($1::uuid[])", uuidStrings(nodeIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[uuid.UUID]string, len(nodeIDs))
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result[id] = name
	}
	return result, rows.Err()
}

// GetAllNodesPaginated gets nodes with pagination.
func (db *Database) GetAllNodesPaginated(ctx context.Context, limit, offset int) ([]*models.Node, int, error) {
	var total int
	err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes").Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+nodeColumns+" FROM nodes ORDER BY registered_at DESC LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes, err := db.rowsToNodes(rows)
	return nodes, total, err
}

// GetNodesScopedPaginated gets nodes filtering by scopes using LIKE ANY.
func (db *Database) GetNodesScopedPaginated(ctx context.Context, limit, offset int, scopes []string) ([]*models.Node, int, error) {
	if len(scopes) == 0 {
		return []*models.Node{}, 0, nil
	}
	likePatterns := make([]string, len(scopes))
	for i, scope := range scopes {
		likePatterns[i] = globToLike(scope)
	}

	var total int
	err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM nodes
		WHERE name LIKE ANY($1::text[])`,
		likePatterns,
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []*models.Node{}, 0, nil
	}

	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+nodeColumns+` FROM nodes
		WHERE name LIKE ANY($3::text[])
		ORDER BY registered_at DESC
		LIMIT $1 OFFSET $2`,
		limit, offset, likePatterns,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes, err := db.rowsToNodes(rows)
	return nodes, total, err
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
	// Visibility filters for scoped users. With node targeting removed, scoped
	// visibility reduces to "own tasks OR all untargeted tasks" (every task is
	// untargeted now), so a scoped user with VisibleToScopes set sees all tasks.
	VisibleToUserID *uuid.UUID
	VisibleToScopes []string
}

// globToLike converts a glob pattern (* and ?) to a SQL LIKE pattern (% and _).
func globToLike(pattern string) string {
	var sb strings.Builder
	for _, c := range pattern {
		switch c {
		case '\\':
			sb.WriteString("\\\\")
		case '%':
			sb.WriteString("\\%")
		case '_':
			sb.WriteString("\\_")
		case '*':
			sb.WriteRune('%')
		case '?':
			sb.WriteRune('_')
		default:
			sb.WriteRune(c)
		}
	}
	return sb.String()
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
			whereClauses = append(whereClauses, fmt.Sprintf("(prompt ILIKE $%d OR CAST(id AS TEXT) ILIKE $%d)", argIndex, argIndex))
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

	// Scoped visibility: with node targeting removed, a scoped user sees their
	// own tasks plus all untargeted tasks — and every task is untargeted now —
	// so this adds no restriction beyond what an unscoped user sees. (Kept as a
	// no-op branch for parity with the handler's call sites.)
	_ = filter.VisibleToScopes

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
