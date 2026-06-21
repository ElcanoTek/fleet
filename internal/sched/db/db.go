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
	"os"
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
}

// New creates a new Database instance.
func New() *Database {
	return &Database{}
}

// Init initializes the database connection and schema. Accepts a connection
// string or reads from DATABASE_URL. A legacy file-path argument (leading '.'
// or '/', or empty) is ignored in favor of DATABASE_URL / DB_* env vars.
func (db *Database) Init(connStr string) error {
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

	db.conn.SetMaxOpenConns(25)
	db.conn.SetMaxIdleConns(5)
	db.conn.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
			AND status IN ($2, $3, $4)
			AND files ? $5
		)`,
		nodeID,
		string(models.TaskStatusAssigned),
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
	if err != sql.ErrNoRows {
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

// GetStaleNodes gets nodes that haven't sent a heartbeat since the cutoff.
func (db *Database) GetStaleNodes(ctx context.Context, cutoff time.Time) ([]*models.Node, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+nodeColumns+" FROM nodes WHERE last_heartbeat < $1 AND status != $2",
		cutoff, string(models.NodeStatusOffline))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToNodes(rows)
}

// GetIdleNodes gets all idle nodes.
func (db *Database) GetIdleNodes(ctx context.Context) ([]*models.Node, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT "+nodeColumns+" FROM nodes WHERE status = $1", string(models.NodeStatusIdle))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return db.rowsToNodes(rows)
}

// Task operations

const taskColumns = "id, prompt, model, fallback_model, max_iterations, mcp_selection, priority, instruction_self_improve, status, assigned_node_id, agent_session_id, created_at, started_at, completed_at, result, error_message, scheduled_for, recurrence, created_by, files, lease_owner, lease_expires_at"

// AddTask adds or updates a task.
func (db *Database) AddTask(ctx context.Context, task *models.Task) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO tasks (
			id, prompt, model, fallback_model, max_iterations, mcp_selection,
			priority, instruction_self_improve, status, assigned_node_id, agent_session_id,
			created_at, started_at, completed_at, result, error_message,
			scheduled_for, recurrence, created_by, files, lease_owner, lease_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (id) DO UPDATE SET
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
			lease_expires_at = EXCLUDED.lease_expires_at`,
		task.ID,
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
	)
	return err
}

func mcpSelectionOrEmpty(s models.MCPSelection) models.MCPSelection {
	if s == nil {
		return models.MCPSelection{}
	}
	return s
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
	)

	err := scanner.Scan(
		&id, &prompt, &model, &fallbackModel, &maxIterations, &mcpSelection,
		&priority, &instructionSelfImprove, &status, &assignedNodeID, &agentSessionID,
		&createdAt, &startedAt, &completedAt, &result, &errorMessage,
		&scheduledFor, &recurrence, &createdBy, &files, &leaseOwner, &leaseExpiresAt,
	)
	if err != nil {
		return nil, err
	}

	task := &models.Task{
		ID:                     id,
		Prompt:                 prompt,
		Priority:               priority,
		InstructionSelfImprove: instructionSelfImprove,
		Status:                 models.TaskStatus(status),
		AssignedNodeID:         assignedNodeID,
		CreatedAt:              createdAt,
		CreatedBy:              createdBy,
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
	if leaseOwner.Valid {
		task.LeaseOwner = &leaseOwner.String
	}
	if leaseExpiresAt.Valid {
		task.LeaseExpiresAt = &leaseExpiresAt.Time
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
	if fromModel != "" {
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3 AND model = $4`,
			model, fallbackModel, string(models.TaskStatusScheduled), fromModel)
	} else {
		res, err = db.conn.ExecContext(ctx, `
			UPDATE tasks SET model = $1, fallback_model = $2
			WHERE status = $3`,
			model, fallbackModel, string(models.TaskStatusScheduled))
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
	defer tx.Rollback()

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
		WHERE status IN ($1, $2, $3, $4)`,
		string(models.TaskStatusRunning),
		string(models.TaskStatusAssigned),
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
			COUNT(*) FILTER (WHERE status IN ($2, $3, $4, $9)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $5 AND completed_at BETWEEN $6 AND $7) as completed_today,
			COUNT(*) FILTER (WHERE status = $8 AND completed_at BETWEEN $6 AND $7) as failed_today
		FROM tasks`,
		string(models.TaskStatusPending),
		string(models.TaskStatusRunning),
		string(models.TaskStatusAssigned),
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
// non-creator tasks fall under the untargeted branch). matchFunc is unused.
func (db *Database) GetDashboardStatsForUser(ctx context.Context, userID *uuid.UUID, scopes []string, _ func(pattern, name string) bool) (*models.DashboardStats, error) {
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
		string(models.TaskStatusAssigned),  // $3
		string(models.TaskStatusAnalyzing), // $4
		string(models.TaskStatusSuccess),   // $5
		todayStart,                         // $6
		todayEnd,                           // $7
		string(models.TaskStatusError),     // $8
		string(models.TaskStatusLeased),    // $9
	)
	whereClause := "TRUE" // all tasks are untargeted → visible to any scoped user
	if userID != nil {
		args = append(args, *userID) // $10
		whereClause = fmt.Sprintf("(created_by = $%d OR TRUE)", len(args))
	}

	query := fmt.Sprintf(`
		SELECT
			COUNT(*) FILTER (WHERE status = $1) as pending_tasks,
			COUNT(*) FILTER (WHERE status IN ($2, $3, $4, $9)) as running_tasks,
			COUNT(*) FILTER (WHERE status = $5 AND completed_at BETWEEN $6 AND $7) as completed_today,
			COUNT(*) FILTER (WHERE status = $8 AND completed_at BETWEEN $6 AND $7) as failed_today
		FROM tasks
		WHERE %s`, whereClause)

	err = db.conn.QueryRowContext(ctx, query, args...).Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasksToday, &stats.FailedTasksToday)
	if err != nil {
		return nil, fmt.Errorf("failed to get task stats: %w", err)
	}
	return stats, nil
}

// Log operations

// AddLog stores a log session for a task.
func (db *Database) AddLog(ctx context.Context, taskID uuid.UUID, session *models.LogSession) error {
	sessionJSON, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `
		INSERT INTO logs (task_id, session_data) VALUES ($1, $2)
		ON CONFLICT (task_id) DO UPDATE SET session_data = EXCLUDED.session_data`,
		taskID, string(sessionJSON))
	return err
}

// GetLog gets the log session for a task.
func (db *Database) GetLog(ctx context.Context, taskID uuid.UUID) (*models.LogSession, error) {
	var sessionData string
	err := db.conn.QueryRowContext(ctx, "SELECT session_data FROM logs WHERE task_id = $1", taskID).Scan(&sessionData)
	if err != nil {
		return nil, err
	}
	var session models.LogSession
	if err := json.Unmarshal([]byte(sessionData), &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// GetAllLogs gets all stored log sessions.
func (db *Database) GetAllLogs(ctx context.Context) (map[uuid.UUID]*models.LogSession, error) {
	rows, err := db.conn.QueryContext(ctx, "SELECT task_id, session_data FROM logs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make(map[uuid.UUID]*models.LogSession)
	for rows.Next() {
		var taskID uuid.UUID
		var sessionData string
		if err := rows.Scan(&taskID, &sessionData); err != nil {
			return nil, err
		}
		var session models.LogSession
		if err := json.Unmarshal([]byte(sessionData), &session); err != nil {
			continue
		}
		logs[taskID] = &session
	}
	return logs, rows.Err()
}

// DeleteOldHistory deletes tasks and logs older than days, in one transaction.
func (db *Database) DeleteOldHistory(ctx context.Context, days int) (int, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days)

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

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
			max_iterations = $22
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
	)
	return err
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

// MarkStaleNodesOfflineBatch marks all stale nodes as offline in one UPDATE.
func (db *Database) MarkStaleNodesOfflineBatch(ctx context.Context, cutoff time.Time) (nodesMarked int, nodeIDsWithTasks []uuid.UUID, err error) {
	result, err := db.conn.ExecContext(ctx, `
		UPDATE nodes SET
			status = $1,
			current_task_id = NULL
		WHERE last_heartbeat < $2
		AND status != $1`,
		string(models.NodeStatusOffline),
		cutoff,
	)
	if err != nil {
		return 0, nil, err
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil, nil
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
			started_at = NULL
		WHERE status IN ($2, $3, $4)
		AND lease_expires_at < $5`,
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
	if err == sql.ErrNoRows {
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
