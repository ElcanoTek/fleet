package dbschema_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/dbschema"
	"github.com/ElcanoTek/fleet/internal/migrations"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// scratchDatabase returns the DSN of a fresh, empty database of the test's
// own (internal/testdb), dropped when the test ends.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	return testdb.Empty(t)
}

// TestBothStoresInOneDatabase is the #2049 regression: both stores, with
// their real migrations, in ONE database from ONE DSN — the configuration
// DATABASE_URL alone resolves to. Each store must live in its own schema,
// see only its own tables, share the ONE identity table in schema core (#2049
// M4: the scheduler's own users table is merged and renamed away) and the
// ONE migration ledger, core.schema_versions (#2049 M6: a fresh database has
// no per-store ledger), and reopen with no migration to run.
func TestBothStoresInOneDatabase(t *testing.T) {
	t.Parallel()

	dsn := scratchDatabase(t)
	ctx := t.Context()
	d, err := config.ResolveDSNs(func(k string) string {
		if k == "DATABASE_URL" {
			return dsn
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolveDSNs: %v", err)
	}
	if err := config.EnsureDistinctStores(d.Chat, d.Sched); err != nil {
		t.Fatalf("EnsureDistinctStores: %v", err)
	}

	open := func() (*store.Store, *scheddb.Database) {
		t.Helper()
		chat, err := store.Open(d.Chat, store.DefaultPoolConfig())
		if err != nil {
			t.Fatalf("open chat store: %v", err)
		}
		sched := &scheddb.Database{}
		if err := sched.Init(d.Sched, scheddb.DefaultPoolConfig()); err != nil {
			_ = chat.Close()
			t.Fatalf("open sched store: %v", err)
		}
		return chat, sched
	}
	chat, sched := open()

	// One account, read by both stores: chat creates it; the Operations
	// Center sees it once it has Operations Center access.
	if _, err := chat.CreateUser(ctx, "alice@example.com", "placeholder-password-1"); err != nil {
		t.Fatalf("chat CreateUser: %v", err)
	}
	cu, err := chat.ListUsers(ctx)
	if err != nil || len(cu) != 1 {
		t.Fatalf("chat ListUsers = %d users, %v; want exactly one", len(cu), err)
	}
	if su, err := sched.ListUsers(ctx); err != nil || len(su) != 0 {
		t.Fatalf("sched ListUsers = %d users, %v; want none before the grant", len(su), err)
	}
	if _, err := sched.Identity().SetOpsAccess(ctx, "alice@example.com", "client"); err != nil {
		t.Fatal(err)
	}
	su, err := sched.ListUsers(ctx)
	if err != nil || len(su) != 1 || su[0].ID.String() != cu[0].ID {
		t.Fatalf("sched ListUsers = %+v, %v; want alice, with the chat row's UUID", su, err)
	}

	// Where everything landed: each store's tables in its schema, nothing in
	// public, and one ledger in core.
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := raw.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	tables := `SELECT count(*) FROM pg_tables WHERE schemaname = $1`
	if n := count(tables, "public"); n != 0 {
		t.Errorf("public holds %d tables, want 0", n)
	}
	for schema, own := range map[string]string{"chat": "conversations", "sched": "tasks"} {
		if count(tables+` AND tablename = $2`, schema, own) != 1 {
			t.Errorf("schema %s lacks its %s table", schema, own)
		}
	}
	if count(tables+` AND tablename = 'tasks'`, "chat") != 0 || count(tables+` AND tablename = 'conversations'`, "sched") != 0 {
		t.Error("a store's table leaked into the other store's schema")
	}
	checkIdentityPlacement(t, count, tables)
	if count(`SELECT count(*) FROM pg_tables WHERE tablename = 'schema_migrations'`) != 0 {
		t.Error("a fresh database grew a legacy schema_migrations ledger")
	}
	if count(tables+` AND tablename = 'schema_versions'`, "core") != 1 {
		t.Error("core.schema_versions is missing")
	}

	// Reopening runs no migration and keeps the data.
	_ = chat.Close()
	_ = sched.Close()
	chat, sched = open()
	defer chat.Close()
	defer sched.Close()
	if rep, err := chat.MigrationStatus(ctx); err != nil || len(rep.Pending) != 0 || rep.Dirty || !rep.Adopted {
		t.Errorf("migrations pending/dirty after reopen: %+v, %v", rep, err)
	}
	if n, err := sched.CountUsers(ctx); err != nil || n != 1 {
		t.Errorf("sched CountUsers after reopen = %d, %v", n, err)
	}
}

// TestEnsureRefusesALegacyDatabase: pointing the one-database layout at a
// legacy store's database must refuse rather than create empty schemas that
// shadow the real data.
func TestEnsureRefusesALegacyDatabase(t *testing.T) {
	t.Parallel()

	dsn := scratchDatabase(t)
	// A legacy store's database: a fleet store's ledger in public (what the
	// probe looks for). The chat store itself can no longer be migrated in
	// public (migration 099 refuses), so the ledger is made by hand.
	{
		raw, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`CREATE TABLE public.schema_migrations (version BIGINT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
	}

	_, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
	if !dbschema.IsLegacyDatabase(err) {
		t.Fatalf("store.Open in schema mode over a legacy database = %v, want the legacy refusal", err)
	}
	if !strings.Contains(err.Error(), "removed two-database layout") {
		t.Errorf("refusal %q does not name the fix", err)
	}
	sched := &scheddb.Database{}
	err = sched.Init(config.WithSchema(dsn, config.SchedSchema), scheddb.DefaultPoolConfig())
	_ = sched.Close()
	if !dbschema.IsLegacyDatabase(err) {
		t.Fatalf("sched Init in schema mode over a legacy database = %v, want the legacy refusal", err)
	}

	// An existing (empty or stray) store schema does not skip the probe: the
	// refusal holds on every open, not only the one that would create it.
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE SCHEMA chat; CREATE SCHEMA sched`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig()); !dbschema.IsLegacyDatabase(err) {
		t.Fatalf("store.Open with an existing chat schema over a legacy database = %v, want the legacy refusal", err)
	}
	sched = &scheddb.Database{}
	err = sched.Init(config.WithSchema(dsn, config.SchedSchema), scheddb.DefaultPoolConfig())
	_ = sched.Close()
	if !dbschema.IsLegacyDatabase(err) {
		t.Fatalf("sched Init with an existing sched schema over a legacy database = %v, want the legacy refusal", err)
	}
}

// TestRunnerIgnoresThePoolSearchPath (#2049 M6): the one migration runner
// sets each migration's search_path itself, so a pool whose DSN names no
// schema (public) migrates the database into chat, core and sched exactly as
// a store's pool does, and creates nothing in public.
func TestRunnerIgnoresThePoolSearchPath(t *testing.T) {
	t.Parallel()

	dsn := scratchDatabase(t)
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := migrations.Up(t.Context(), raw); err != nil {
		t.Fatalf("Up through a public pool: %v", err)
	}
	var public, inSchemas int
	if err := raw.QueryRow(`SELECT count(*) FILTER (WHERE schemaname = 'public'),
		count(*) FILTER (WHERE schemaname IN ('chat', 'core', 'sched')) FROM pg_tables`).Scan(&public, &inSchemas); err != nil {
		t.Fatal(err)
	}
	if public != 0 || inSchemas < 50 {
		t.Fatalf("tables: %d in public, %d in chat/core/sched", public, inSchemas)
	}
}

// checkIdentityPlacement: the identity table is core.users and nowhere else;
// the scheduler's legacy table is merged and renamed (#2049 M4).
func checkIdentityPlacement(t *testing.T, count func(string, ...any) int, tables string) {
	t.Helper()
	// The identity table: in core, nowhere else; the scheduler's legacy table
	// merged and renamed.
	if count(tables+` AND tablename = 'users'`, "core") != 1 || count(tables+` AND tablename = 'users'`, "chat") != 0 ||
		count(tables+` AND tablename = 'users'`, "sched") != 0 || count(tables+` AND tablename = 'users_premerge_m4'`, "sched") != 1 {
		t.Error("users is not exactly core.users (with sched.users renamed users_premerge_m4)")
	}
	if count(`SELECT count(*) FROM core.identity_merge WHERE step = 'm4_sched_users'`) != 1 {
		t.Error("the identity merge did not record itself")
	}
}

// TestSchemasMatchConfig: dbschema's list of fleet schemas is config's three
// names, in byte order (the readiness check reports findings in that order).
func TestSchemasMatchConfig(t *testing.T) {
	t.Parallel()
	want := []string{config.ChatSchema, config.IdentitySchema, config.SchedSchema}
	if strings.Join(dbschema.Schemas, ",") != strings.Join(want, ",") {
		t.Fatalf("dbschema.Schemas = %v, want %v", dbschema.Schemas, want)
	}
}
