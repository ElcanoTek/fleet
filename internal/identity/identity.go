// Package identity is fleet's one identity table (#2049 M4, ADR-0105,
// docs/IDENTITY.md).
//
// A person has ONE row in core.users, keyed by their (lowercased) email, with
// a stable UUID, one Fleet role (admin | member | viewer) and an orthogonal
// Operations Center scope (none | view | run). Chat and the Operations Center
// read the same row: there is no second users table, no shadow sync between
// planes and no lookup seam carrying an identity from one store to the other.
//
// The table lives in the neutral schema core of the one database (ADR-0098).
// Chat migration 099 moved the chat users table there. Every statement in this
// package names its tables schema-qualified (core.users), so a connection of
// either store — whose search_path names only its own schema — reads the same
// rows.
//
// The model, and how it maps onto the Operations Center's old role names:
//
//   - role admin implies full Operations Center access; ops_scope is ignored;
//   - otherwise ops_scope run grants create_task, view_tasks, view_logs
//     (the old "client"), view grants view_tasks, view_logs (the old
//     "readonly"), none grants nothing;
//   - a disabled account (central Auth revoked it) has no access to either
//     plane, whatever its role and scope.
//
// The external contracts keep the legacy names: LegacyOpsRole renders a
// row as admin | client | readonly ("" for no access), and ParseOpsRole
// accepts both vocabularies.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Role is a person's one Fleet role.
type Role string

// The three roles. They match the CHECK constraint on core.users.role
// (chat migration 024).
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// ValidRole reports whether r names a role.
func ValidRole(r string) bool {
	switch Role(r) {
	case RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

// OpsScope is the Operations Center access a non-admin holds.
type OpsScope string

// The three scopes. They match the CHECK constraint on core.users.ops_scope
// (chat migration 099).
const (
	OpsNone OpsScope = "none"
	OpsView OpsScope = "view"
	OpsRun  OpsScope = "run"
)

// ValidOpsScope reports whether s names a scope.
func ValidOpsScope(s string) bool {
	switch OpsScope(s) {
	case OpsNone, OpsView, OpsRun:
		return true
	}
	return false
}

// The Operations Center's legacy role names: what its API, `fleet sched user`,
// API keys and Auth's ops_role setting still speak.
const (
	LegacyOpsAdmin    = "admin"
	LegacyOpsClient   = "client"
	LegacyOpsReadonly = "readonly"
	LegacyOpsNone     = "none"
)

// LegacyOpsRole renders role and scope as the Operations Center role the old
// sched users table would have held: "admin" for an admin, "client" for run,
// "readonly" for view, and "" for no Operations Center access. It does not
// look at whether the account is enabled; User.LegacyOpsRole does.
func LegacyOpsRole(role Role, scope OpsScope) string {
	if role == RoleAdmin {
		return LegacyOpsAdmin
	}
	switch scope {
	case OpsRun:
		return LegacyOpsClient
	case OpsView:
		return LegacyOpsReadonly
	case OpsNone:
		return ""
	}
	return ""
}

// ParseOpsRole reads an Operations Center grant in either vocabulary:
// none | readonly | client | admin (legacy) or none | view | run (ops_scope).
// admin reports admin=true and scope none: Operations Center admin is the
// Fleet admin role, not a scope. ok is false for anything else.
//
// Matching is exact, like ValidRole and ValidOpsScope: there is one spelling
// of each grant, so a caller comparing the raw input against a constant
// (e.g. refusing "none") sees the same value the parser accepted. " none " or
// "Admin" are refused, not trimmed or folded.
func ParseOpsRole(s string) (scope OpsScope, admin, ok bool) {
	switch s {
	case LegacyOpsNone:
		return OpsNone, false, true
	case LegacyOpsReadonly, string(OpsView):
		return OpsView, false, true
	case LegacyOpsClient, string(OpsRun):
		return OpsRun, false, true
	case LegacyOpsAdmin:
		return OpsNone, true, true
	}
	return "", false, false
}

// User is one row of core.users. It never carries the password hash.
type User struct {
	// ID is the stable UUID the scheduler records as a task's or dataset's
	// creator (tasks.created_by, datasets.created_by_user_id).
	ID    uuid.UUID
	Email string
	Role  Role
	// OpsScope is the stored scope; read EffectiveOpsScope or LegacyOpsRole
	// for what it grants.
	OpsScope OpsScope
	TeamID   string
	Enabled  bool
	// SessionEpoch is the revocation generation a password-minted session
	// cookie must carry (ADR-0041; SessionEpochSQL).
	SessionEpoch string
	LastLogin    *time.Time
	CreatedAt    int64
	UpdatedAt    int64
}

// HasOpsAccess reports whether the account may use the Operations Center:
// enabled, and an admin or a non-admin with a scope.
func (u *User) HasOpsAccess() bool {
	return u != nil && u.Enabled && (u.Role == RoleAdmin || u.OpsScope == OpsView || u.OpsScope == OpsRun)
}

// EffectiveOpsScope is what the Operations Center grants this account, for
// a non-admin: its scope when enabled, none otherwise. An admin reports run
// (the widest scope); callers that care about admin check Role.
func (u *User) EffectiveOpsScope() OpsScope {
	if !u.HasOpsAccess() {
		return OpsNone
	}
	if u.Role == RoleAdmin {
		return OpsRun
	}
	return u.OpsScope
}

// LegacyOpsRole is the account's effective Operations Center role in the
// legacy names: "" when it has no access (disabled, or a non-admin with no
// scope).
func (u *User) LegacyOpsRole() string {
	if !u.HasOpsAccess() {
		return ""
	}
	return LegacyOpsRole(u.Role, u.OpsScope)
}

// ErrNotFound is returned when no row matches.
var ErrNotFound = errors.New("identity: user not found")

// ErrExists is returned when a create finds the email taken.
var ErrExists = errors.New("already exists")

// InputError is a caller's mistake (a bad role, a missing email): HTTP 400,
// CLI exit 1.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

func inputErr(format string, args ...any) error {
	return &InputError{msg: fmt.Sprintf(format, args...)}
}

// IsInputError reports whether err is an InputError.
func IsInputError(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}

// NormalizeEmail lowercases and trims an email (or a legacy sched username):
// every key into core.users goes through it.
func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// SessionEpochBytes is how much of the digest makes up a session epoch
// (store.sessionEpochFor is the Go side of the same derivation).
const SessionEpochBytes = 8

// SessionEpochSQL derives a row's session epoch in SQL over core.users'
// password_hash and session_salt (ADR-0041). It is the one copy of the
// derivation both planes read: the chat membership check and the Operations
// Center's principal check (principal.Resolve) compare a cookie's claim
// against this value.
// store.TestSessionEpochSQLMatchesGo pins it to the Go derivation.
var SessionEpochSQL = fmt.Sprintf(
	`encode(substring(sha256(convert_to(password_hash || session_salt, 'UTF8')) FROM 1 FOR %d), 'hex')`,
	SessionEpochBytes)

// SessionEpochFor is SessionEpochSQL in Go: the epoch of a stored bcrypt
// digest and session salt.
func SessionEpochFor(bcryptDigest, sessionSalt string) string {
	sum := sha256.Sum256([]byte(bcryptDigest + sessionSalt))
	return hex.EncodeToString(sum[:SessionEpochBytes])
}

// NoAccountEpoch is the epoch reported for an email with no row.
func NoAccountEpoch() string { return SessionEpochFor("", "") }

// LegacyChat is a chat account as it stood before the one identity table (nil: none).
type LegacyChat struct {
	Role    Role
	Enabled bool
}

// LegacySched is an Operations Center account as it stood before the one identity table.
type LegacySched struct {
	Role    string // admin | client | readonly (the old column had no CHECK)
	Enabled bool
}

// Merged is what MapLegacy decides for one account.
type Merged struct {
	Role     Role
	OpsScope OpsScope
	Enabled  bool
	// Created: there was no chat account; the merge creates one.
	Created bool
	// SplitAdmin: an Operations Center admin without chat admin, failed
	// closed to scope run (ADR-0074).
	SplitAdmin bool
	// UnknownRole: the sched role was none of admin, client, readonly.
	UnknownRole bool
}

// MapLegacy is the one-identity-table mapping (docs/IDENTITY.md) for one
// person: their chat account (nil when they had none) and their Operations
// Center account. It is pure, so the authorization differential test runs
// every state through the same decision.
func MapLegacy(chat *LegacyChat, sched LegacySched) Merged {
	role := sched.Role
	scope, adminGrant, known := ParseOpsRole(role)
	out := Merged{}
	if !known || role == LegacyOpsNone || role == string(OpsView) || role == string(OpsRun) {
		out.UnknownRole = true
		scope, adminGrant = OpsNone, false
	}
	if !sched.Enabled {
		scope, adminGrant = OpsNone, false
	}
	if chat == nil {
		out.Created, out.Role, out.Enabled = true, RoleViewer, sched.Enabled
		if adminGrant {
			out.SplitAdmin, scope = true, OpsRun
		}
		out.OpsScope = scope
		return out
	}
	out.Role, out.Enabled = chat.Role, chat.Enabled
	switch {
	case chat.Role == RoleAdmin:
		out.OpsScope = OpsNone
	case adminGrant:
		out.SplitAdmin, out.OpsScope = true, OpsRun
	default:
		out.OpsScope = scope
	}
	return out
}
