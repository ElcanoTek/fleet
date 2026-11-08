package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MergeStep is the core.identity_merge key of the one-time fold of the
// scheduler's legacy users table into core.users.
const MergeStep = "m4_sched_users"

// PremergeTable is the name the scheduler's legacy users table is renamed to
// once merged. Nothing reads it; a later release drops it.
const PremergeTable = "users_premerge_m4"

// MergeReport is what MergeLegacySchedUsers did. It is stored as JSON in
// core.identity_merge.report and printed in the log of the open that ran the
// merge (docs/IDENTITY.md).
type MergeReport struct {
	// Merged counts sched accounts folded into an existing chat account.
	Merged int `json:"merged"`
	// Created lists sched-only accounts that became new rows (role viewer,
	// with their Operations Center scope): a small, deliberate widening —
	// such a person can now also sign in to an empty chat.
	Created []MergeEntry `json:"created,omitempty"`
	// DemotedSplitAdmins lists Operations Center admins whose chat role was
	// not admin. ADR-0074: split admin states fail closed, so they keep their
	// chat role and get ops_scope run; re-grant with `fleet admin add`.
	DemotedSplitAdmins []MergeEntry `json:"demoted_split_admins,omitempty"`
	// UnknownRoles lists sched rows whose role was none of admin, client,
	// readonly (the old column had no CHECK): they get no Operations Center
	// access.
	UnknownRoles []MergeEntry `json:"unknown_roles,omitempty"`
	// AdminsGainingOps lists chat admins that had no enabled Operations
	// Center admin row: admin is one role now, so they gain full Operations
	// Center access (the owner-accepted model; docs/IDENTITY.md).
	AdminsGainingOps []MergeEntry `json:"admins_gaining_ops,omitempty"`
	// DuplicateUsernames lists sched usernames that differ only in case from
	// one merged earlier in the same run: the first keeps the UUID, and tasks
	// created by the others no longer resolve to an account.
	DuplicateUsernames []MergeEntry `json:"duplicate_usernames,omitempty"`
	// DanglingTaskCreators / DanglingDatasetCreators count non-null creator
	// UUIDs that resolve to no account after the merge (rows a deleted
	// account left behind before this release).
	DanglingTaskCreators    int `json:"dangling_task_creators"`
	DanglingDatasetCreators int `json:"dangling_dataset_creators"`
	// At is when the merge ran.
	At time.Time `json:"at"`
}

// MergeEntry names one account in a MergeReport.
type MergeEntry struct {
	Email     string `json:"email"`
	SchedRole string `json:"sched_role,omitempty"`
	ChatRole  string `json:"chat_role,omitempty"`
	OpsScope  string `json:"ops_scope,omitempty"`
}

// Notable reports whether the report names anything an operator should read.
func (r *MergeReport) Notable() bool {
	return r != nil && (len(r.Created) > 0 || len(r.DemotedSplitAdmins) > 0 || len(r.UnknownRoles) > 0 || len(r.AdminsGainingOps) > 0 ||
		len(r.DuplicateUsernames) > 0 || r.DanglingTaskCreators > 0 || r.DanglingDatasetCreators > 0)
}

// Lines renders the report for a log or a terminal.
func (r *MergeReport) Lines() []string {
	if r == nil {
		return nil
	}
	out := []string{fmt.Sprintf("identity merge: %d Operations Center account(s) merged into existing accounts, %d created", r.Merged, len(r.Created))}
	for _, e := range r.Created {
		out = append(out, fmt.Sprintf("  created %s (Operations Center only before; now role viewer, ops_scope %s)", e.Email, e.OpsScope))
	}
	for _, e := range r.DemotedSplitAdmins {
		out = append(out, fmt.Sprintf("  NOT an admin any more: %s was an Operations Center admin with chat role %s; kept role %s with ops_scope run. Re-grant with `fleet admin add %s` if intended.", e.Email, e.ChatRole, e.ChatRole, e.Email))
	}
	for _, e := range r.AdminsGainingOps {
		out = append(out, fmt.Sprintf("  %s is a chat admin with no Operations Center admin row before: admin is one role now, so they have full Operations Center access", e.Email))
	}
	for _, e := range r.UnknownRoles {
		out = append(out, fmt.Sprintf("  %s had unknown Operations Center role %q: no Operations Center access", e.Email, e.SchedRole))
	}
	for _, e := range r.DuplicateUsernames {
		out = append(out, fmt.Sprintf("  %s: another Operations Center username differing only in case was merged first; this one's tasks no longer resolve to an account", e.Email))
	}
	if r.DanglingTaskCreators > 0 || r.DanglingDatasetCreators > 0 {
		out = append(out, fmt.Sprintf("  %d task(s) and %d dataset(s) name a creator that is no longer an account (deleted before this release)", r.DanglingTaskCreators, r.DanglingDatasetCreators))
	}
	return out
}

// MergeResult is MergeLegacySchedUsers' outcome.
type MergeResult struct {
	// Report is nil when the merge has not run yet and could not run now
	// (Ready false).
	Report *MergeReport
	// Ran is true when this call did the merge (false: it was already done).
	Ran bool
	// Ready is false when the database is not ready for the merge: the chat
	// store has not created core.users yet, or the scheduler has not created
	// its schema. Nothing was written; a later call (the next store open)
	// merges.
	Ready bool
}

// mergeLockSQL serializes merges across processes (a CLI and the server
// starting together).
const mergeLockSQL = `SELECT pg_advisory_xact_lock(hashtext('fleet.identity.merge'))`

// MergeLegacySchedUsers folds the scheduler's legacy users table (schedSchema
// .users, the Operations Center accounts before this release) into
// core.users, once. It is idempotent — guarded by the core.identity_merge
// row MergeStep — and transactional: the whole fold, the rename of the
// legacy table to PremergeTable and the report commit together or not at
// all. It runs on every scheduler store open (sched/db.Init); the first call
// that finds both stores migrated does the work.
//
// Mapping (docs/IDENTITY.md):
//
//   - a sched row whose lowercased username matches a core.users email
//     gives that row the sched UUID (so tasks.created_by and
//     datasets.created_by_user_id keep resolving) and its scope: client →
//     run, readonly → view, a disabled row → none; a chat admin stays admin;
//     an Operations Center admin whose chat role is not admin keeps the chat
//     role with scope run, and is reported (ADR-0074: fail closed);
//   - the chat password is kept, the sched one dropped;
//   - a sched row with no chat row becomes a new row keyed by the lowercased
//     username, role viewer, the mapped scope, enabled as it was, keeping the
//     sched UUID and password hash; it is reported.
//
// While it runs it holds an exclusive lock on the legacy table and blocks
// writes to core.users (SHARE ROW EXCLUSIVE), so a concurrent account create
// waits for it rather than racing a duplicate.
func MergeLegacySchedUsers(ctx context.Context, db *sql.DB, schedSchema string) (MergeResult, error) {
	if !plainIdentifier(schedSchema) {
		return MergeResult{}, fmt.Errorf("identity merge: scheduler schema %q is not a plain identifier", schedSchema)
	}
	// Cheap path first: one read, no lock, once the merge has happened.
	if rep, done, err := readMarker(ctx, db); err != nil || done {
		return MergeResult{Report: rep, Ready: err == nil}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MergeResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := MergeTx(ctx, tx, schedSchema)
	if err != nil || !res.Ran {
		return res, err
	}
	return res, tx.Commit()
}

// readMarker returns the stored report when the merge has run. A database
// without core.identity_merge (the chat store not migrated yet) reports not
// done and no error.
func readMarker(ctx context.Context, q Querier) (*MergeReport, bool, error) {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('core.identity_merge') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT report FROM core.identity_merge WHERE step = $1`, MergeStep).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var rep MergeReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, true, fmt.Errorf("identity merge report: %w", err)
	}
	return &rep, true, nil
}

// LastMergeReport returns the stored merge report, or nil when the merge has
// not run.
func LastMergeReport(ctx context.Context, db *sql.DB) (*MergeReport, error) {
	rep, _, err := readMarker(ctx, db)
	return rep, err
}

// LegacyChat is a chat account as it stood before the merge (nil: none).
type LegacyChat struct {
	Role    Role
	Enabled bool
}

// LegacySched is an Operations Center account as it stood before the merge.
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

// MapLegacy is the merge's mapping (docs/IDENTITY.md) for one person: their
// chat account (nil when they had none) and their Operations Center account.
// It is pure, so the authorization differential test runs every state
// through the same decision the merge applies.
func MapLegacy(chat *LegacyChat, sched LegacySched) Merged {
	role := strings.TrimSpace(sched.Role)
	scope, adminGrant, known := ParseOpsRole(role)
	out := Merged{}
	if !known || role == LegacyOpsNone || role == string(OpsView) || role == string(OpsRun) {
		// Only the three legacy names were ever written; anything else grants
		// nothing.
		out.UnknownRole = true
		scope, adminGrant = OpsNone, false
	}
	if !sched.Enabled {
		scope, adminGrant = OpsNone, false
	}
	if chat == nil {
		// Operations-Center-only account: a new row, role viewer, enabled as
		// the sched row was. An admin has no chat role to be admin with:
		// fail closed like a split admin.
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
		// A chat admin is a Fleet admin: the scope is ignored.
		out.OpsScope = OpsNone
	case adminGrant:
		out.SplitAdmin, out.OpsScope = true, OpsRun
	default:
		out.OpsScope = scope
	}
	return out
}

type legacyRow struct {
	id        uuid.UUID
	username  string
	hash      string
	role      string
	enabled   bool
	lastLogin sql.NullTime
}

// MergeTx is MergeLegacySchedUsers inside the caller's transaction (which it
// does not commit). Exposed for tests that hold the merge open.
func MergeTx(ctx context.Context, tx *sql.Tx, schedSchema string) (MergeResult, error) {
	if !plainIdentifier(schedSchema) {
		return MergeResult{}, fmt.Errorf("identity merge: scheduler schema %q is not a plain identifier", schedSchema)
	}
	if _, err := tx.ExecContext(ctx, mergeLockSQL); err != nil {
		return MergeResult{}, err
	}
	// Re-check under the lock: another process may have merged meanwhile.
	if rep, done, err := readMarker(ctx, tx); err != nil || done {
		return MergeResult{Report: rep, Ready: err == nil}, err
	}
	var coreReady, schedReady bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('core.users') IS NOT NULL AND to_regclass('core.identity_merge') IS NOT NULL,
		to_regclass($1) IS NOT NULL`, schedSchema+".schema_migrations").Scan(&coreReady, &schedReady); err != nil {
		return MergeResult{}, err
	}
	if !coreReady || !schedReady {
		return MergeResult{Ready: false}, nil
	}
	legacy := schedSchema + ".users"
	var hasLegacy bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, legacy).Scan(&hasLegacy); err != nil {
		return MergeResult{}, err
	}
	rep := &MergeReport{At: time.Now().UTC()}
	if hasLegacy {
		if err := foldLegacy(ctx, tx, schedSchema, rep); err != nil {
			return MergeResult{}, err
		}
	}
	if err := countDangling(ctx, tx, schedSchema, rep); err != nil {
		return MergeResult{}, err
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return MergeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO core.identity_merge (step, done_at, report) VALUES ($1, now(), $2)`, MergeStep, raw); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{Report: rep, Ran: true, Ready: true}, nil
}

// foldLegacy is the merge proper; see MergeLegacySchedUsers.
func foldLegacy(ctx context.Context, tx *sql.Tx, schedSchema string, rep *MergeReport) error {
	legacy := schedSchema + ".users"
	// Identifiers passed plainIdentifier; DDL takes no bind parameters.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+legacy+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE core.users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, username, password_hash, role, enabled, last_login FROM `+legacy+` ORDER BY created_at, id`) //nolint:gosec // G202: see above
	if err != nil {
		return err
	}
	var legacyRows []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.username, &r.hash, &r.role, &r.enabled, &r.lastLogin); err != nil {
			_ = rows.Close()
			return err
		}
		legacyRows = append(legacyRows, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	seen := map[string]bool{}
	opsAdmins := map[string]bool{}
	now := time.Now().Unix()
	for _, r := range legacyRows {
		email := NormalizeEmail(r.username)
		if email == "" {
			continue
		}
		schedRole := strings.TrimSpace(r.role)
		if seen[email] {
			rep.DuplicateUsernames = append(rep.DuplicateUsernames, MergeEntry{Email: email, SchedRole: schedRole})
			continue
		}
		seen[email] = true
		if r.enabled && schedRole == LegacyOpsAdmin {
			opsAdmins[email] = true
		}

		var chat *LegacyChat
		var chatRole string
		switch err := tx.QueryRowContext(ctx, `SELECT role FROM core.users WHERE email = $1 FOR UPDATE`, email).Scan(&chatRole); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			chat = &LegacyChat{Role: Role(chatRole)}
		}
		m := MapLegacy(chat, LegacySched{Role: schedRole, Enabled: r.enabled})
		if m.UnknownRole {
			rep.UnknownRoles = append(rep.UnknownRoles, MergeEntry{Email: email, SchedRole: schedRole})
		}
		if m.SplitAdmin {
			rep.DemotedSplitAdmins = append(rep.DemotedSplitAdmins, MergeEntry{Email: email, SchedRole: schedRole, ChatRole: string(m.Role), OpsScope: string(m.OpsScope)})
		}
		if m.Created {
			hash := r.hash
			if hash == "" {
				hash = UnusablePasswordHash
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO core.users (id, email, password_hash, role, ops_scope, enabled, last_login, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, r.id, email, hash, string(m.Role), string(m.OpsScope), m.Enabled, r.lastLogin, now); err != nil {
				return fmt.Errorf("identity merge: create %s: %w", email, err)
			}
			rep.Created = append(rep.Created, MergeEntry{Email: email, SchedRole: schedRole, ChatRole: string(m.Role), OpsScope: string(m.OpsScope)})
			continue
		}
		// The chat row keeps its role, enabled state and password; it takes the
		// sched UUID (tasks and datasets keep resolving) and the scope.
		if _, err := tx.ExecContext(ctx, `UPDATE core.users SET id = $2, ops_scope = $3,
			last_login = GREATEST(last_login, $4) WHERE email = $1`, email, r.id, string(m.OpsScope), r.lastLogin); err != nil {
			return fmt.Errorf("identity merge: merge %s: %w", email, err)
		}
		rep.Merged++
	}
	// Chat admins that were not Operations Center admins gain that access
	// now: report them, so the widening is visible.
	admins, err := tx.QueryContext(ctx, `SELECT email FROM core.users WHERE role = 'admin' AND enabled ORDER BY email`)
	if err != nil {
		return err
	}
	for admins.Next() {
		var email string
		if err := admins.Scan(&email); err != nil {
			_ = admins.Close()
			return err
		}
		if !opsAdmins[email] {
			rep.AdminsGainingOps = append(rep.AdminsGainingOps, MergeEntry{Email: email, ChatRole: string(RoleAdmin)})
		}
	}
	if err := admins.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+legacy+` RENAME TO `+PremergeTable); err != nil {
		return err
	}
	return nil
}

// countDangling counts creator UUIDs that resolve to no account.
func countDangling(ctx context.Context, tx *sql.Tx, schedSchema string, rep *MergeReport) error {
	for _, c := range []struct {
		table, column string
		into          *int
	}{
		{"tasks", "created_by", &rep.DanglingTaskCreators},
		{"datasets", "created_by_user_id", &rep.DanglingDatasetCreators},
	} {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`, schedSchema, c.table, c.column).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		q := `SELECT COUNT(*) FROM ` + schedSchema + `.` + c.table + ` t WHERE t.` + c.column + ` IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM core.users u WHERE u.id = t.` + c.column + `)`
		if err := tx.QueryRowContext(ctx, q).Scan(c.into); err != nil {
			return err
		}
	}
	return nil
}

// plainIdentifier reports whether s is a lower-case SQL identifier that
// needs no quoting.
func plainIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
