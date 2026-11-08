package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// Querier is what the identity reads and writes need: *sql.DB and *sql.Tx
// both satisfy it, so a caller can fold an identity write into its own
// transaction.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store reads and writes core.users over a connection of either store. Its
// statements are schema-qualified, so the connection's search_path does not
// matter.
type Store struct{ db *sql.DB }

// New returns a Store over db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB is the underlying pool.
func (s *Store) DB() *sql.DB { return s.db }

// columns is the read projection behind every *User: never password_hash.
var columns = "id, email, role, ops_scope, COALESCE(team_id, ''), enabled, " + SessionEpochSQL + ", last_login, created_at, updated_at"

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var (
		u         User
		role      string
		scope     string
		lastLogin sql.NullTime
	)
	if err := row.Scan(&u.ID, &u.Email, &role, &scope, &u.TeamID, &u.Enabled, &u.SessionEpoch, &lastLogin, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	u.Role, u.OpsScope = Role(role), OpsScope(scope)
	if lastLogin.Valid {
		t := lastLogin.Time
		u.LastLogin = &t
	}
	return &u, nil
}

func one(row *sql.Row) (*User, error) {
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

// GetByEmail returns email's row whatever its state (enabled or not). Access
// decisions read HasOpsAccess / Enabled on the result.
func (s *Store) GetByEmail(ctx context.Context, email string) (*User, error) {
	return GetByEmail(ctx, s.db, email)
}

// GetByEmail is Store.GetByEmail on any Querier (a transaction).
func GetByEmail(ctx context.Context, q Querier, email string) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, ErrNotFound
	}
	return one(q.QueryRowContext(ctx, `SELECT `+columns+` FROM core.users WHERE email = $1`, email))
}

// GetByID returns the row with that UUID whatever its state.
func (s *Store) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return one(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM core.users WHERE id = $1`, id))
}

// List returns every row, ordered by email.
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM core.users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]User, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// ListOpsAccess returns the rows that hold Operations Center access in their
// stored role and scope (an admin, or a scope other than none), enabled or
// not, ordered by email: the accounts `fleet sched user list` shows.
func (s *Store) ListOpsAccess(ctx context.Context) ([]User, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, u := range all {
		if LegacyOpsRole(u.Role, u.OpsScope) != "" {
			out = append(out, u)
		}
	}
	return out, nil
}

// EmailsByID maps each id that has a row to its email.
func (s *Store) EmailsByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, email FROM core.users WHERE id = ANY($1::uuid[])`, strs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id uuid.UUID
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, err
		}
		out[id] = email
	}
	return out, rows.Err()
}

// SessionEpoch returns the password-session epoch a cookie minted for email
// must carry (ADR-0041). An email with no row reports the epoch of an empty
// hash — a value no row can hold — so a session minted for an address that is
// not an account stops working the moment one is created.
func (s *Store) SessionEpoch(ctx context.Context, email string) (string, error) {
	var epoch string
	err := s.db.QueryRowContext(ctx, `SELECT `+SessionEpochSQL+` FROM core.users WHERE email = $1`, NormalizeEmail(email)).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return NoAccountEpoch(), nil
	}
	return epoch, err
}

// LookupExternalSessionEpoch is the request-path read of a central (OIDC)
// identity's session generation: "" when the identity never signed in here,
// which matches no cookie claim (fails closed). It never writes.
func (s *Store) LookupExternalSessionEpoch(ctx context.Context, issuer, subject string) (string, error) {
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if issuer == "" || subject == "" {
		return "", errors.New("issuer and subject are required")
	}
	var epoch string
	err := s.db.QueryRowContext(ctx,
		`SELECT epoch FROM core.external_auth_epochs WHERE issuer = $1 AND subject = $2`, issuer, subject).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return epoch, err
}

// SetOpsAccess applies an Operations Center grant in either vocabulary to an
// existing row, in one statement:
//
//   - "admin" makes the account a Fleet admin (Operations Center admin IS
//     the Fleet admin role);
//   - any other value sets ops_scope, and is refused for an admin (an admin's
//     Operations Center access is not a scope: demote the role first).
//
// The account is not enabled or disabled here.
func (s *Store) SetOpsAccess(ctx context.Context, email, grant string) (*User, error) {
	return SetOpsAccess(ctx, s.db, email, grant)
}

// SetOpsAccess is Store.SetOpsAccess on any Querier (a transaction).
func SetOpsAccess(ctx context.Context, q Querier, email, grant string) (*User, error) {
	scope, admin, ok := ParseOpsRole(grant)
	if !ok {
		return nil, inputErr("invalid Operations Center role %q (want none|view|run, or admin|client|readonly)", grant)
	}
	email = NormalizeEmail(email)
	now := time.Now().Unix()
	var res sql.Result
	var err error
	if admin {
		res, err = q.ExecContext(ctx, `UPDATE core.users SET role = 'admin', ops_scope = 'none', updated_at = $2 WHERE email = $1`, email, now)
	} else {
		var cur string
		switch err := q.QueryRowContext(ctx, `SELECT role FROM core.users WHERE email = $1 FOR UPDATE`, email).Scan(&cur); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, ErrNotFound
		case err != nil:
			return nil, err
		}
		if Role(cur) == RoleAdmin {
			return nil, inputErr("%s is a Fleet admin, which includes full Operations Center access; change their role first", email)
		}
		res, err = q.ExecContext(ctx, `UPDATE core.users SET ops_scope = $2, updated_at = $3 WHERE email = $1`, email, string(scope), now)
	}
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return GetByEmail(ctx, q, email)
}

// AddOpsAccess is what "create an Operations Center user" means on the one
// identity table (the admin-key POST /users, `fleet sched user add`): before
// it, the Operations Center had its own users, so an email that was already a
// chat account could still be added there. Now that account is the same row:
// when it is enabled and has no Operations Center access yet, the grant is
// applied to it (its password and chat role untouched, except that "admin"
// makes it a Fleet admin). An account that already has
// Operations Center access, or is disabled, is left alone: ErrExists. A
// missing account is ErrNotFound (the caller creates it).
func (s *Store) AddOpsAccess(ctx context.Context, email, grant string) (u *User, err error) {
	if _, _, ok := ParseOpsRole(grant); !ok {
		return nil, inputErr("invalid Operations Center role %q", grant)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	email = NormalizeEmail(email)
	var role, scope string
	var enabled bool
	switch err := tx.QueryRowContext(ctx, `SELECT role, ops_scope, enabled FROM core.users WHERE email = $1 FOR UPDATE`, email).Scan(&role, &scope, &enabled); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	cur := User{Email: email, Role: Role(role), OpsScope: OpsScope(scope), Enabled: enabled}
	if !enabled || cur.HasOpsAccess() {
		return nil, fmt.Errorf("user %s %w", email, ErrExists)
	}
	if u, err = SetOpsAccess(ctx, tx, email, grant); err != nil {
		return nil, err
	}
	return u, tx.Commit()
}

// SetOpsScope sets a non-admin row's scope (see SetOpsAccess).
func (s *Store) SetOpsScope(ctx context.Context, email string, scope OpsScope) (*User, error) {
	if !ValidOpsScope(string(scope)) {
		return nil, inputErr("invalid ops_scope %q (want none|view|run)", scope)
	}
	return s.SetOpsAccess(ctx, email, string(scope))
}

// SetRole sets a row's Fleet role. Demoting an admin leaves the stored scope
// as it is (none unless it was set while the account was not an admin), so
// the demotion also ends Operations Center admin access.
func (s *Store) SetRole(ctx context.Context, email string, role Role) (*User, error) {
	if !ValidRole(string(role)) {
		return nil, inputErr("invalid role %q (want admin|member|viewer)", role)
	}
	email = NormalizeEmail(email)
	res, err := s.db.ExecContext(ctx, `UPDATE core.users SET role = $2, updated_at = $3 WHERE email = $1`, email, string(role), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetByEmail(ctx, email)
}

// NewAccount is a row CreateAccount inserts.
type NewAccount struct {
	Email string
	// PasswordHash is a bcrypt hash, or UnusablePasswordHash for an account
	// that signs in only through a session cookie (central Auth).
	PasswordHash string
	Role         Role
	OpsScope     OpsScope
	// ID is the row's UUID; zero picks a new one.
	ID uuid.UUID
	// CreatedAt is when the account was made; zero means now. An import
	// (fleet import of a legacy sched bundle) carries the source's.
	CreatedAt time.Time
	// LastLogin is the last sign-in the source recorded; nil for none.
	LastLogin *time.Time
}

// UnusablePasswordHash is stored for an account that has no Fleet password.
// It is deliberately not a bcrypt string, so no password ever matches it.
const UnusablePasswordHash = "!central-auth-only!" //nolint:gosec // G101: a sentinel that is deliberately NOT a credential (no password matches it)

// CreateAccount inserts a row. ErrExists (wrapped) when the email is taken.
func (s *Store) CreateAccount(ctx context.Context, a NewAccount) (*User, error) {
	return CreateAccount(ctx, s.db, a)
}

// CreateAccount is Store.CreateAccount on any Querier (a transaction).
func CreateAccount(ctx context.Context, q Querier, a NewAccount) (*User, error) {
	a.Email = NormalizeEmail(a.Email)
	if a.Email == "" {
		return nil, inputErr("email required")
	}
	if a.Role == "" {
		a.Role = RoleMember
	}
	if a.OpsScope == "" {
		a.OpsScope = OpsNone
	}
	if !ValidRole(string(a.Role)) || !ValidOpsScope(string(a.OpsScope)) {
		return nil, inputErr("invalid role %q / ops_scope %q", a.Role, a.OpsScope)
	}
	if a.PasswordHash == "" {
		a.PasswordHash = UnusablePasswordHash
	}
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	now := time.Now().Unix()
	created := now
	if !a.CreatedAt.IsZero() {
		created = a.CreatedAt.Unix()
	}
	var lastLogin any
	if a.LastLogin != nil && !a.LastLogin.IsZero() {
		lastLogin = a.LastLogin.UTC()
	}
	_, err := q.ExecContext(ctx, `INSERT INTO core.users (id, email, password_hash, role, ops_scope, created_at, updated_at, last_login)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, a.ID, a.Email, a.PasswordHash, string(a.Role), string(a.OpsScope), created, now, lastLogin)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("user %s %w", a.Email, ErrExists)
		}
		return nil, err
	}
	return GetByEmail(ctx, q, a.Email)
}

// EnsureAdmin makes email a Fleet admin, creating the row when it is missing
// (with no usable password: the account signs in through a session cookie)
// and re-enabling it when it is disabled. It is the FLEET_ORCHESTRATOR_
// BOOTSTRAP_ADMINS boot seed: config-authoritative and idempotent.
func (s *Store) EnsureAdmin(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	if email == "" {
		return nil
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO core.users (id, email, password_hash, role, ops_scope, created_at, updated_at)
		VALUES ($1, $2, $3, 'admin', 'none', $4, $4)
		ON CONFLICT (email) DO UPDATE SET role = 'admin', enabled = TRUE, updated_at = EXCLUDED.updated_at
		WHERE core.users.role <> 'admin' OR NOT core.users.enabled`,
		uuid.New(), email, UnusablePasswordHash, now)
	return err
}

// RecordLogin stamps last_login.
func (s *Store) RecordLogin(ctx context.Context, email string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE core.users SET last_login = $2 WHERE email = $1`, NormalizeEmail(email), at.UTC())
	return err
}

// HashPassword bcrypt-hashes a password for CreateAccount, refusing one
// shorter than 8 characters (the rule every Fleet password write applies).
func HashPassword(plain string, cost int) (string, error) {
	if len(plain) < 8 {
		return "", inputErr("password must be at least 8 characters")
	}
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// SetPassword stores a new bcrypt hash for email. A password write moves the
// session epoch (SessionEpochSQL), so every password session of the account
// ends (ADR-0041).
func (s *Store) SetPassword(ctx context.Context, email, bcryptHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE core.users SET password_hash = $1, updated_at = $2 WHERE email = $3`,
		bcryptHash, time.Now().Unix(), NormalizeEmail(email))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
