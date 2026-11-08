package admincli

import (
	"context"
	"database/sql"
	osexec "os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// preMergeDatabase is a box between its first start on the one identity
// table's release and the scheduler's identity merge: every migration applied
// (#2049 M6: one runner migrates chat, core and sched together), the
// scheduler's legacy accounts still in sched.users, the merge not run. Each
// email is both a chat account and a legacy Operations Center account.
func preMergeDatabase(t *testing.T, emails ...string) string {
	t.Helper()
	dsn := testdb.Empty(t)
	if err := migrations.UpDSN(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	chat, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range emails {
		if _, err := raw.Exec(`INSERT INTO sched.users (id, username, password_hash, role, created_at, enabled) VALUES ($1, $2, 'sched-hash-placeholder', 'client', now(), true)`, uuid.New(), e); err != nil {
			t.Fatal(err)
		}
		if _, err := chat.CreateUser(context.Background(), e, "placeholder-not-a-secret"); err != nil {
			t.Fatal(err)
		}
	}
	_ = chat.Close()
	var pending bool
	if err := raw.QueryRow(`SELECT to_regclass('sched.users') IS NOT NULL AND to_regclass('core.users') IS NOT NULL`).Scan(&pending); err != nil || !pending {
		t.Fatalf("not a pre-merge layout (%v)", err)
	}
	return dsn
}

// The first operator verb after an upgrade completes the identity merge
// before it touches an account, so a removed account stays removed: the
// scheduler's next start finds nothing to fold back in (Codex 4505570159 on
// #2083).
func TestAccountVerbsCompleteThePendingMerge(t *testing.T) {
	dsn := preMergeDatabase(t, "gone@x.test", "removed@x.test")
	chatURL := config.WithSchema(dsn, config.ChatSchema)

	if code := cmdChat([]string{"user", "del", "gone@x.test", "--database-url", chatURL}); code != 0 {
		t.Fatalf("chat user del: exit %d", code)
	}
	if code := cmdAdmin([]string{"add", "removed@x.test", "--password", "placeholder-not-a-secret", "--chat-database-url", chatURL}); code != 0 {
		t.Fatalf("admin add: exit %d", code)
	}
	if code := cmdAdmin([]string{"rm", "removed@x.test", "--chat-database-url", chatURL}); code != 0 {
		t.Fatalf("admin rm: exit %d", code)
	}

	// The first scheduler start (fleet serve).
	st := storage.New()
	if err := st.Initialize(config.WithSchema(dsn, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatalf("scheduler start: %v", err)
	}
	defer st.Close()
	if m := st.DB().IdentityMerge(); !m.Ready || m.Ran {
		t.Fatalf("the merge should have completed in the first verb, not now: %+v", m)
	}
	if u, err := st.GetIdentity(t.Context(), "gone@x.test"); err == nil && u != nil {
		t.Fatalf("a deleted account came back after the scheduler started: %+v", u)
	}
	if u, err := st.GetIdentity(t.Context(), "removed@x.test"); err == nil && u != nil {
		t.Fatalf("admin rm was undone: %+v", u)
	}
}

// A sched dump from after the merge, restored over a database whose own merge
// is pending, would leave sched.users next to the restored
// sched.users_premerge_m4 and wedge the scheduler. The restore completes the
// pending merge first when it can (the chat store migrated), and refuses
// when it cannot (Codex 4505570164 on #2083).
func TestSchedRestoreOverAPendingMerge(t *testing.T) {
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := osexec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	src := fleetdb.New(t)
	schedDump := filepath.Join(t.TempDir(), "sched.dump")
	if err := runPgDump(ctx, src.SchedDSN(), schedDump); err != nil {
		t.Fatal(err)
	}

	// (Before #2049 M6 a scheduler could migrate without the chat store, so
	// a pending merge could be impossible to complete here; now any open
	// migrates chat too, and the merge can always complete.)
	// The chat store migrated: the merge completes, then the restore runs and
	// the scheduler opens on the result.
	pre := preMergeDatabase(t, "ops@x.test")
	if err := runPgRestoreWith(ctx, config.WithSchema(pre, config.SchedSchema), schedDump, restoreOpts{allowUnresolved: true}); err != nil {
		t.Fatalf("sched restore over a completable pending merge: %v", err)
	}
	var legacy bool
	if err := sqlOne(t, pre, `SELECT to_regclass('sched.users') IS NOT NULL`, &legacy); err != nil || legacy {
		t.Fatalf("sched.users still there after the restore (%v)", err)
	}
	st := storage.New()
	if err := st.Initialize(config.WithSchema(pre, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatalf("scheduler on the restored database: %v", err)
	}
	st.Close()
}

// A box from before M6 whose chat store migrated (core.users exists) but whose
// scheduler is still behind sched 072 (no sched.users.enabled yet), restored
// with `restore --db=sched` of a dump from after the merge: the pending merge
// must not run against the half-migrated scheduler. The restore migrates the
// database first (adopting its legacy ledgers and applying 072–077), then
// merges, then restores (Codex 4506035624 on #2087).
func TestSchedRestoreMigratesBeforeMerging(t *testing.T) {
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := osexec.LookPath(b); err != nil {
			t.Skipf("%s not found", b)
		}
	}
	ctx := context.Background()
	src := fleetdb.New(t)
	schedDump := filepath.Join(t.TempDir(), "sched.dump")
	if err := runPgDump(ctx, src.SchedDSN(), schedDump); err != nil {
		t.Fatal(err)
	}

	// The pre-M6 layout: chat 1–99 and sched 1–71 applied and recorded in the
	// two legacy ledgers, no core.schema_versions, sched.users unmerged.
	dsn := testdb.Empty(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := migrations.UpTo(ctx, raw, migrations.SchedOffset+71); err != nil {
		t.Fatal(err)
	}
	set, err := migrations.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	mustRaw := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustRaw(`DROP TABLE core.schema_versions`)
	mustRaw(`CREATE TABLE chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`)
	for _, m := range set.All() {
		if m.Stream == migrations.StreamChat {
			mustRaw(`INSERT INTO chat.schema_migrations VALUES ($1, $2, 1700000000)`, m.LegacyVersion(), m.LegacyName())
		}
	}
	mustRaw(`CREATE TABLE sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`)
	mustRaw(`INSERT INTO sched.schema_migrations VALUES (71, false)`)
	var hasEnabled bool
	if err := raw.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'sched' AND table_name = 'users' AND column_name = 'enabled')`).Scan(&hasEnabled); err != nil || hasEnabled {
		t.Fatalf("not a scheduler behind 072 (enabled=%v, %v)", hasEnabled, err)
	}
	mustRaw(`INSERT INTO sched.users (id, username, password_hash, role, created_at) VALUES ($1, 'ops@x.test', 'h', 'client', now())`, uuid.New())
	mustRaw(`INSERT INTO core.users (email, password_hash, created_at, updated_at) VALUES ('ops@x.test', 'h', 1, 1)`)

	if err := runPgRestoreWith(ctx, config.WithSchema(dsn, config.SchedSchema), schedDump, restoreOpts{allowUnresolved: true}); err != nil {
		t.Fatalf("sched restore over a pre-M6 scheduler behind 072: %v", err)
	}
	var top int
	var legacy, merged bool
	if err := raw.QueryRow(`SELECT (SELECT max(version) FROM core.schema_versions),
		to_regclass('sched.users') IS NOT NULL,
		EXISTS (SELECT 1 FROM core.identity_merge WHERE step = 'm4_sched_users')`).Scan(&top, &legacy, &merged); err != nil {
		t.Fatal(err)
	}
	if top != migrations.LegacyLatest || legacy || !merged {
		t.Fatalf("after the restore: ledger at %d, sched.users left %v, merged %v; want %d, false, true", top, legacy, merged, migrations.LegacyLatest)
	}
	st := storage.New()
	if err := st.Initialize(config.WithSchema(dsn, config.SchedSchema), fleetdb.SchedPool()); err != nil {
		t.Fatalf("scheduler on the restored database: %v", err)
	}
	st.Close()
}
