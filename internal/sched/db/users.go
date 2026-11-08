package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Operations Center accounts (#2049 M4, ADR-0105).
//
// The scheduler has no users table of its own any more: an Operations Center
// account is a row of the one identity table, core.users, read through
// internal/identity. A models.User here is that row seen from this plane —
// Username is the email, ID the row's UUID (what tasks.created_by records) and
// Role the legacy Operations Center role (admin | client | readonly) the row's
// role and ops_scope grant, so userHasPermission and the role checks read
// exactly as they did over the old table.

// Identity returns the identity store over this database's pool.
func (db *Database) Identity() *identity.Store { return identity.New(db.conn) }

// toModel renders an identity row as this plane's user. Role is the legacy
// Operations Center role, "" for an account without access.
func toModel(u *identity.User) *models.User {
	out := &models.User{
		ID:        u.ID,
		Username:  u.Email,
		Role:      u.LegacyOpsRole(),
		CreatedAt: time.Unix(u.CreatedAt, 0).UTC(),
	}
	if u.LastLogin != nil {
		t := *u.LastLogin
		out.LastLogin = &t
	}
	return out
}

// opsUser returns the account if it may use the Operations Center (enabled,
// and an admin or a non-admin with a scope), else sql.ErrNoRows — the
// "enabled row" lookup the old table answered.
func opsUser(u *identity.User, err error) (*models.User, error) {
	if errors.Is(err, identity.ErrNotFound) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if !u.HasOpsAccess() {
		return nil, sql.ErrNoRows
	}
	return toModel(u), nil
}

// GetUser returns the account with that UUID if it has Operations Center
// access, else sql.ErrNoRows.
func (db *Database) GetUser(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	return opsUser(db.Identity().GetByID(ctx, userID))
}

// GetUserByUsername returns the account (username = lowercased email) if it
// has Operations Center access, else sql.ErrNoRows. Membership and every
// owner lookup go through it.
func (db *Database) GetUserByUsername(ctx context.Context, username string) (*models.User, error) {
	return opsUser(db.Identity().GetByEmail(ctx, username))
}

// GetIdentity returns the account's identity row whatever its access (nil
// and identity.ErrNotFound when there is none): a lookup for the CLI and the
// admin paths. (The request path resolves a person with principal.Resolve.)
func (db *Database) GetIdentity(ctx context.Context, email string) (*identity.User, error) {
	return db.Identity().GetByEmail(ctx, email)
}

// ListUsers returns the accounts that hold Operations Center access in their
// stored role and scope, enabled or not, ordered by username: what `fleet
// sched user list` shows. A disabled account lists with its stored role.
func (db *Database) ListUsers(ctx context.Context) ([]models.User, error) {
	rows, err := db.Identity().ListOpsAccess(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.User, 0, len(rows))
	for i := range rows {
		m := toModel(&rows[i])
		m.Role = identity.LegacyOpsRole(rows[i].Role, rows[i].OpsScope)
		out = append(out, *m)
	}
	return out, nil
}

// CountUsers returns the number of accounts ListUsers lists.
func (db *Database) CountUsers(ctx context.Context) (int, error) {
	rows, err := db.Identity().ListOpsAccess(ctx)
	return len(rows), err
}

// GetUsersByIDs maps each UUID that names an account to its email (the
// board's Created By column). Any account resolves, with or without
// Operations Center access: a task's creator stays named after their access
// ends.
func (db *Database) GetUsersByIDs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return db.Identity().EmailsByID(ctx, userIDs)
}
