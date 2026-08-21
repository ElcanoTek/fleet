package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

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
