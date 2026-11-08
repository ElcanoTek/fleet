package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/migrations"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// scratchDatabase returns a fresh, empty database of the test's own
// (internal/testdb), dropped when the test ends. Each merge test gets its own
// so the legacy sched users table it seeds is exactly the one the merge folds.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	return testdb.Empty(t)
}

// legacy is a database as a box upgraded to this release looks before its
// first full boot: the sched store migrated (its users table still holding
// the Operations Center accounts), the chat store not yet moved to core. The
// sched store opens first, so its open-time merge finds no core.users and
// does nothing; the test seeds both tables, then migrates the chat store and
// runs the merge itself.
type legacy struct {
	t    *testing.T
	dsn  string
	raw  *sql.DB
	chat *store.Store
}

func newLegacy(t *testing.T) *legacy {
	t.Helper()
	dsn := scratchDatabase(t)
	l := &legacy{t: t, dsn: dsn}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	l.raw = raw
	// The database as the release before the merge left it: every migration
	// applied (#2049 M6: one runner), the scheduler's legacy users table still
	// in place, the merge not run (it runs at the scheduler's open, which
	// these tests stand in for with merge()).
	if err := migrations.Up(context.Background(), raw); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var merged bool
	if err := raw.QueryRow(`SELECT EXISTS (SELECT 1 FROM core.identity_merge)`).Scan(&merged); err != nil || merged {
		t.Fatalf("merge ran before the test seeded the legacy accounts: %v, %v", merged, err)
	}
	return l
}

// openSched opens the scheduler store (after the test's merge: its own
// open-time merge is then one read), closed when the test ends.
func (l *legacy) openSched() *scheddb.Database {
	l.t.Helper()
	sd := &scheddb.Database{}
	if err := sd.Init(config.WithSchema(l.dsn, config.SchedSchema), scheddb.DefaultPoolConfig()); err != nil {
		l.t.Fatalf("open sched: %v", err)
	}
	l.t.Cleanup(func() { _ = sd.Close() })
	return sd
}

func (l *legacy) schedUser(id uuid.UUID, username, role string, enabled bool) {
	l.t.Helper()
	if _, err := l.raw.Exec(`INSERT INTO sched.users (id, username, password_hash, role, created_at, enabled, last_login)
		VALUES ($1, $2, 'sched-hash-placeholder', $3, now(), $4, now())`, id, username, role, enabled); err != nil {
		l.t.Fatalf("seed sched user %s: %v", username, err)
	}
}

// openChat migrates (099 moves users into core) and opens the chat store.
func (l *legacy) openChat() *store.Store {
	l.t.Helper()
	if l.chat == nil {
		st, err := store.Open(config.WithSchema(l.dsn, config.ChatSchema), store.DefaultPoolConfig())
		if err != nil {
			l.t.Fatalf("open chat: %v", err)
		}
		l.t.Cleanup(func() { _ = st.Close() })
		l.chat = st
	}
	return l.chat
}

// chatUser creates a chat account with a role.
func (l *legacy) chatUser(email, role string) {
	l.t.Helper()
	st := l.openChat()
	if _, err := st.CreateUser(context.Background(), email, "chat-password-placeholder"); err != nil {
		l.t.Fatalf("chat user %s: %v", email, err)
	}
	if _, err := st.SetUserRoleTeam(context.Background(), email, &role, nil); err != nil {
		l.t.Fatal(err)
	}
}

func (l *legacy) merge() identity.MergeResult {
	l.t.Helper()
	l.openChat()
	res, err := identity.MergeLegacySchedUsers(context.Background(), l.raw, config.SchedSchema)
	if err != nil {
		l.t.Fatalf("merge: %v", err)
	}
	return res
}

func (l *legacy) account(email string) *identity.User {
	l.t.Helper()
	u, err := identity.New(l.raw).GetByEmail(context.Background(), email)
	if err != nil {
		l.t.Fatalf("account %s: %v", email, err)
	}
	return u
}

func names(es []identity.MergeEntry) string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Email)
	}
	return strings.Join(out, ",")
}

// TestMergeMapsEveryState is the mapping table of docs/IDENTITY.md, row by
// row, plus UUID preservation (a task created by a merged account still
// resolves to it), the report, the rename, and the chat epoch staying put.
func TestMergeMapsEveryState(t *testing.T) {
	l := newLegacy(t)
	ids := map[string]uuid.UUID{}
	for _, n := range []string{"admin", "client", "readonly", "disabled", "split", "only", "onlyadmin", "weird", "dupe"} {
		ids[n] = uuid.New()
	}
	// sched rows
	l.schedUser(ids["admin"], "Admin@X.test", "admin", true)
	l.schedUser(ids["client"], "client@x.test", "client", true)
	l.schedUser(ids["readonly"], "readonly@x.test", "readonly", true)
	l.schedUser(ids["disabled"], "disabled@x.test", "client", false)
	l.schedUser(ids["split"], "split@x.test", "admin", true)
	l.schedUser(ids["only"], "ci-bot", "client", true)
	l.schedUser(ids["onlyadmin"], "opsadmin@x.test", "admin", true)
	l.schedUser(ids["weird"], "weird@x.test", "superuser", true)
	l.schedUser(ids["dupe"], "CLIENT@x.test", "readonly", true)
	// A task created by the merged client, and one by an account deleted
	// before this release (its row long gone).
	for _, by := range []uuid.UUID{ids["client"], uuid.New()} {
		if _, err := l.raw.Exec(`INSERT INTO sched.tasks (id, prompt, priority, status, created_at, created_by)
			VALUES ($1, 'p', 0, $2, now(), $3)`, uuid.New(), string(models.TaskStatusPending), by); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	// chat rows
	l.chatUser("admin@x.test", "admin")
	l.chatUser("client@x.test", "member")
	l.chatUser("readonly@x.test", "viewer")
	l.chatUser("disabled@x.test", "member")
	l.chatUser("split@x.test", "member")
	l.chatUser("weird@x.test", "member")
	l.chatUser("chatonly@x.test", "member")
	l.chatUser("chatadmin@x.test", "admin")
	epochBefore, err := l.chat.SessionEpoch(context.Background(), "client@x.test")
	if err != nil {
		t.Fatal(err)
	}

	res := l.merge()
	if !res.Ran || !res.Ready || res.Report == nil {
		t.Fatalf("merge result = %+v", res)
	}
	rep := res.Report

	for _, c := range []struct {
		email, role, scope, legacy string
		id                         uuid.UUID
	}{
		{"admin@x.test", "admin", "none", "admin", ids["admin"]},
		{"client@x.test", "member", "run", "client", ids["client"]},
		{"readonly@x.test", "viewer", "view", "readonly", ids["readonly"]},
		{"disabled@x.test", "member", "none", "", ids["disabled"]},
		// Fail closed (ADR-0074): Operations Center admin without chat admin.
		{"split@x.test", "member", "run", "client", ids["split"]},
		// Operations-Center-only accounts become viewers.
		{"ci-bot", "viewer", "run", "client", ids["only"]},
		{"opsadmin@x.test", "viewer", "run", "client", ids["onlyadmin"]},
		{"weird@x.test", "member", "none", "", ids["weird"]},
	} {
		u := l.account(c.email)
		if string(u.Role) != c.role || string(u.OpsScope) != c.scope || u.LegacyOpsRole() != c.legacy || u.ID != c.id {
			t.Errorf("%s = role %s scope %s ops %q id %s; want %s/%s/%q/%s", c.email, u.Role, u.OpsScope, u.LegacyOpsRole(), u.ID, c.role, c.scope, c.legacy, c.id)
		}
	}
	if u := l.account("chatonly@x.test"); u.OpsScope != identity.OpsNone || u.LegacyOpsRole() != "" {
		t.Errorf("chat-only account got Operations Center access: %+v", u)
	}
	// The chat password stays; an Operations-Center-only account keeps its
	// sched hash (it can sign in with its old password).
	if err := l.chat.VerifyUser(context.Background(), "client@x.test", "chat-password-placeholder"); err != nil {
		t.Errorf("merged account lost its chat password: %v", err)
	}
	var hash string
	if err := l.raw.QueryRow(`SELECT password_hash FROM core.users WHERE email = 'ci-bot'`).Scan(&hash); err != nil || hash != "sched-hash-placeholder" {
		t.Errorf("sched-only account hash = %q, %v", hash, err)
	}
	// The chat epoch is unchanged: no chat session is signed out.
	if after, err := l.chat.SessionEpoch(context.Background(), "client@x.test"); err != nil || after != epochBefore {
		t.Errorf("chat epoch moved: %q → %q (%v)", epochBefore, after, err)
	}
	checkReport(t, rep)
	// UUID preservation: the merged client's task resolves to the account.
	var creator string
	if err := l.raw.QueryRow(`SELECT u.email FROM sched.tasks t JOIN core.users u ON u.id = t.created_by`).Scan(&creator); err != nil || creator != "client@x.test" {
		t.Errorf("task creator = %q, %v", creator, err)
	}
	// The legacy table is renamed away, and the marker stored with the report.
	var legacyTable, premerge bool
	if err := l.raw.QueryRow(`SELECT to_regclass('sched.users') IS NOT NULL, to_regclass('sched.users_premerge_m4') IS NOT NULL`).Scan(&legacyTable, &premerge); err != nil || legacyTable || !premerge {
		t.Errorf("sched.users present=%v, users_premerge_m4 present=%v (%v)", legacyTable, premerge, err)
	}
	if stored, err := identity.LastMergeReport(context.Background(), l.raw); err != nil || stored == nil || stored.Merged != 6 {
		t.Errorf("stored report = %+v, %v", stored, err)
	}
}

// TestMergeIsIdempotent: a second run (another process, the next boot) does
// nothing and returns the stored report; a fresh install (empty sched.users)
// renames the table and records an empty report.
func TestMergeIsIdempotent(t *testing.T) {
	l := newLegacy(t)
	l.chatUser("a@x.test", "member")
	l.schedUser(uuid.New(), "a@x.test", "client", true)
	first := l.merge()
	second := l.merge()
	if !first.Ran || second.Ran || !second.Ready || second.Report == nil || second.Report.Merged != 1 {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if u := l.account("a@x.test"); u.OpsScope != identity.OpsRun {
		t.Fatalf("second run changed the row: %+v", u)
	}
	// And the scheduler's own open-time merge is the same no-op.
	sd := &scheddb.Database{}
	if err := sd.Init(config.WithSchema(l.dsn, config.SchedSchema), scheddb.DefaultPoolConfig()); err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	if m := sd.IdentityMerge(); m.Ran || !m.Ready {
		t.Fatalf("open-time merge after the merge = %+v", m)
	}

	fresh := newLegacy(t)
	if res := fresh.merge(); !res.Ran || res.Report.Merged != 0 || res.Report.Notable() {
		t.Fatalf("fresh install merge = %+v", res)
	}
}

// TestMergeIsTransactional: a failing merge leaves nothing half-done — no
// account touched, the legacy table in place, no marker — so the next open
// retries it.
func TestMergeIsTransactional(t *testing.T) {
	l := newLegacy(t)
	l.chatUser("a@x.test", "member")
	l.chatUser("b@x.test", "member")
	l.chatUser("c@x.test", "member")
	l.schedUser(uuid.New(), "a@x.test", "client", true)
	// b's sched UUID is already c's: the UPDATE that gives b its sched UUID
	// violates core.users' unique id, after a@ was already merged.
	var taken uuid.UUID
	if err := l.raw.QueryRow(`SELECT id FROM core.users WHERE email = 'c@x.test'`).Scan(&taken); err != nil {
		t.Fatal(err)
	}
	l.schedUser(taken, "b@x.test", "readonly", true)
	if _, err := identity.MergeLegacySchedUsers(context.Background(), l.raw, config.SchedSchema); err == nil {
		t.Fatal("a merge with a UUID collision succeeded")
	}
	if u := l.account("a@x.test"); u.OpsScope != identity.OpsNone {
		t.Errorf("a failed merge left a@ changed: %+v", u)
	}
	var legacyTable bool
	var markers int
	if err := l.raw.QueryRow(`SELECT to_regclass('sched.users') IS NOT NULL, (SELECT count(*) FROM core.identity_merge)`).Scan(&legacyTable, &markers); err != nil || !legacyTable || markers != 0 {
		t.Errorf("after a failed merge: legacy table %v, markers %d (%v)", legacyTable, markers, err)
	}
}

// TestMergeBlocksConcurrentAccountWrites: while the merge holds its locks, an
// account create for an email the merge is about to create waits for it and
// then finds the account (ErrUserExists) — it never races a duplicate.
func TestMergeBlocksConcurrentAccountWrites(t *testing.T) {
	l := newLegacy(t)
	l.openChat()
	l.schedUser(uuid.New(), "late@x.test", "client", true)

	tx, err := l.raw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := identity.MergeTx(context.Background(), tx, config.SchedSchema)
	if err != nil || !res.Ran {
		t.Fatalf("merge in tx: %+v %v", res, err)
	}

	var wg sync.WaitGroup
	var createErr error
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, createErr = l.chat.CreateUser(context.Background(), "late@x.test", "chat-password-placeholder")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("an account create ran while the merge held its locks")
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !errors.Is(createErr, store.ErrUserExists) {
		t.Fatalf("create after the merge = %v, want ErrUserExists", createErr)
	}
	if u := l.account("late@x.test"); u.Role != identity.RoleViewer || u.OpsScope != identity.OpsRun {
		t.Fatalf("merged account = %+v", u)
	}
}

// checkReport asserts TestMergeMapsEveryState's report.
func checkReport(t *testing.T, rep *identity.MergeReport) {
	t.Helper()
	if rep.Merged != 6 || names(rep.Created) != "ci-bot,opsadmin@x.test" ||
		names(rep.DemotedSplitAdmins) != "split@x.test,opsadmin@x.test" || names(rep.UnknownRoles) != "weird@x.test" ||
		names(rep.DuplicateUsernames) != "client@x.test" || rep.DanglingTaskCreators != 1 ||
		names(rep.AdminsGainingOps) != "chatadmin@x.test" {
		t.Errorf("report = %+v", rep)
	}
	if !rep.Notable() || len(rep.Lines()) < 5 {
		t.Errorf("report lines = %q", rep.Lines())
	}
}

// TestMergeKeepsSchedOnlyAccountHistory: an Operations-Center-only account the
// merge creates keeps its sched created_at and last_login; only updated_at
// is the merge's moment (Codex 4504789445 on #2083).
func TestMergeKeepsSchedOnlyAccountHistory(t *testing.T) {
	l := newLegacy(t)
	created := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	lastLogin := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	id := uuid.New()
	if _, err := l.raw.Exec(`INSERT INTO sched.users (id, username, password_hash, role, created_at, enabled, last_login)
		VALUES ($1, 'old-ops@x.test', 'sched-hash-placeholder', 'client', $2, true, $3)`, id, created, lastLogin); err != nil {
		t.Fatalf("seed sched user: %v", err)
	}
	before := time.Now().Unix()
	if res := l.merge(); !res.Ran || len(res.Report.Created) != 1 {
		t.Fatalf("merge = %+v", res)
	}
	u := l.account("old-ops@x.test")
	if u.CreatedAt != created.Unix() {
		t.Errorf("created_at = %s, want the sched row's %s", time.Unix(u.CreatedAt, 0).UTC(), created)
	}
	if u.LastLogin == nil || !u.LastLogin.Equal(lastLogin) {
		t.Errorf("last_login = %v, want %s", u.LastLogin, lastLogin)
	}
	if u.UpdatedAt < before {
		t.Errorf("updated_at = %d, want the merge's moment (>= %d)", u.UpdatedAt, before)
	}
}
