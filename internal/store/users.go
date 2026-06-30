package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// User is a provisioned operator-added account. Passwords live here as
// bcrypt hashes; plaintext is only surfaced at creation time by the
// admin CLI and never logged.
type User struct {
	Email string
	// Role is one of "member" (default), "viewer" (read-only), or "admin" (#237).
	// A user in the ADMIN_EMAILS env allowlist is treated as admin regardless of
	// this column (an out-of-band bootstrap gate); see Server.isAdmin.
	Role string
	// TeamID groups users into a read-sharing trust group. Empty = no team. A
	// teammate sees a conversation only once its owner opts in (team_visible),
	// so a shared team_id never auto-exposes private history.
	TeamID    string
	CreatedAt int64
	UpdatedAt int64
}

// Role constants (#237). Kept in sync with the CHECK constraint in
// migrations/024_rbac.sql.
const (
	RoleMember = "member"
	RoleViewer = "viewer"
	RoleAdmin  = "admin"
)

// ValidRole reports whether r is a recognized role.
func ValidRole(r string) bool {
	switch r {
	case RoleMember, RoleViewer, RoleAdmin:
		return true
	default:
		return false
	}
}

// ErrUserNotFound is returned by VerifyUser / GetUser when the email is
// absent. Login handlers should treat this indistinguishably from a
// wrong password so enumeration attacks can't probe the allowlist.
var ErrUserNotFound = errors.New("user not found")

// ErrBadPassword is returned by VerifyUser when the hash check fails.
var ErrBadPassword = errors.New("bad password")

// normalizeEmail lowercases + trims whitespace. Case-insensitive
// uniqueness is enforced at the app layer: every email goes through
// this function before hitting the DB, so the plain TEXT PRIMARY KEY
// is sufficient without depending on the CITEXT extension.
func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

// pgUniqueViolation reports whether err wraps a Postgres unique-
// constraint violation (SQLSTATE 23505).
func pgUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateUser inserts a new user. Errors if the email already exists.
func (s *Store) CreateUser(ctx context.Context, email, plainPassword string) (*User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return nil, errors.New("email required")
	}
	if len(plainPassword) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, updated_at) VALUES ($1, $2, $3, $4)`,
		email, string(hash), now, now,
	)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("user %s already exists", email)
		}
		return nil, err
	}
	// role defaults to 'member' via the column default (#237); reflect that here.
	return &User{Email: email, Role: RoleMember, CreatedAt: now, UpdatedAt: now}, nil
}

// GetUser returns the full user record (role + team included). Returns
// ErrUserNotFound when the email isn't provisioned — the membership middleware
// uses this to admit known users AND enrich the request with their role/team.
func (s *Store) GetUser(ctx context.Context, email string) (*User, error) {
	email = normalizeEmail(email)
	if email == "" {
		return nil, ErrUserNotFound
	}
	var u User
	var teamID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT email, role, team_id, created_at, updated_at FROM users WHERE email = $1`, email).
		Scan(&u.Email, &u.Role, &teamID, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	u.TeamID = teamID.String
	return &u, nil
}

// SetUserRoleTeam applies a partial role/team update (PATCH /admin/users/{email},
// #237): a nil pointer leaves that column untouched, a non-nil one writes it (an
// empty team_id string clears the team → NULL). Validates role against the CHECK
// set. Returns the updated user, or ErrUserNotFound when the email is absent.
func (s *Store) SetUserRoleTeam(ctx context.Context, email string, role, teamID *string) (*User, error) {
	email = normalizeEmail(email)
	if role != nil && !ValidRole(*role) {
		return nil, fmt.Errorf("invalid role %q (want member|viewer|admin)", *role)
	}
	// COALESCE($n, current) leaves a column untouched when its arg is NULL; an
	// empty team_id is written as SQL NULL so "clear the team" round-trips.
	var teamArg any
	if teamID != nil {
		if strings.TrimSpace(*teamID) == "" {
			teamArg = nil // explicit clear
		} else {
			teamArg = strings.TrimSpace(*teamID)
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET role     = COALESCE($2, role),
		    team_id  = CASE WHEN $4::bool THEN $3 ELSE team_id END,
		    updated_at = $5
		WHERE email = $1`,
		email, role, teamArg, teamID != nil, time.Now().Unix(),
	)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrUserNotFound
	}
	return s.GetUser(ctx, email)
}

// dummyPasswordHash is bcrypt-compared against on the unknown-email
// path so a failed login costs one full bcrypt verification whether or
// not the email exists. Without it, the not-found path returns in
// microseconds while a wrong password costs ~50-100ms — a timing oracle
// that enumerates the provisioned allowlist from the public login form.
// Generated (not a literal) so it stays cost-matched with CreateUser.
var dummyPasswordHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer-dummy"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

// VerifyUser returns nil iff the provided plaintext matches the stored
// hash. Returns ErrUserNotFound / ErrBadPassword for the two failure
// modes — callers should reveal neither to the client, through message
// content or timing.
func (s *Store) VerifyUser(ctx context.Context, email, plainPassword string) error {
	email = normalizeEmail(email)
	row := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE email = $1`, email)
	var hash string
	if err := row.Scan(&hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Burn the same bcrypt work a real comparison costs.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(plainPassword))
			return ErrUserNotFound
		}
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)); err != nil {
		return ErrBadPassword
	}
	return nil
}

// IsUser reports whether email belongs to a provisioned user. The
// membership middleware (scoped-tier gate) uses it to admit only known
// users who signed in via the shared elcano_auth cookie — chat keeps
// owning WHO may use chat while auth proves WHO they are. The lookup is a
// single indexed PK hit; email is normalized so it matches the same way
// VerifyUser does.
func (s *Store) IsUser(ctx context.Context, email string) (bool, error) {
	email = normalizeEmail(email)
	if email == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE email = $1`, email).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpdatePassword rotates a user's password. Used by `chat user passwd`.
func (s *Store) UpdatePassword(ctx context.Context, email, plainPassword string) error {
	email = normalizeEmail(email)
	if len(plainPassword) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = $1, updated_at = $2 WHERE email = $3`,
		string(hash), time.Now().Unix(), email,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// DeleteUser removes a user by email AND their conversations. The
// on-disk workspace directories belonging to those conversations are
// reaped on the next SweepOrphanWorkspaces run (post-turn, from any
// user), so uploaded attachments and scratch files don't outlive the
// account that produced them.
//
// The cascade is done in a single transaction so a partial failure
// can't leave orphan rows; the workspace sweep is best-effort and
// happens separately, off the hot path.
func (s *Store) DeleteUser(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM conversations WHERE user_email = $1`, email); err != nil {
		return fmt.Errorf("delete conversations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memories WHERE user_email = $1`, email); err != nil {
		return fmt.Errorf("delete memories: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE email = $1`, email)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUserNotFound
	}
	return tx.Commit()
}

// ListUsers returns every provisioned user, sorted by email.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, role, team_id, created_at, updated_at FROM users ORDER BY email ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var teamID sql.NullString
		if err := rows.Scan(&u.Email, &u.Role, &teamID, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.TeamID = teamID.String
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of provisioned users. The HTTP layer
// uses this to decide whether the instance is "unprovisioned" (0 users)
// and should therefore reject every login attempt with a dedicated
// error telling the operator to run `chat user add`.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
