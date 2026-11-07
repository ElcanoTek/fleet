package dbschema_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/dbschema"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
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
// see only its own tables (both have a `users` and a `schema_migrations`
// table of incompatible shapes), and reopen with no migration to run.
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

	// The cross-store shape that broke the old shared database: a user in
	// each store's `users` table, under the same name.
	if _, err := chat.CreateUser(ctx, "alice@example.com", "placeholder-password-1"); err != nil {
		t.Fatalf("chat CreateUser: %v", err)
	}
	if err := sched.AddUser(ctx, &models.User{ID: uuid.New(), Username: "alice@example.com", PasswordHash: "x", Role: "user", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("sched AddUser: %v", err)
	}
	cu, err := chat.ListUsers(ctx)
	if err != nil || len(cu) != 1 {
		t.Fatalf("chat ListUsers = %d users, %v; want exactly its own one", len(cu), err)
	}
	su, err := sched.ListUsers(ctx)
	if err != nil || len(su) != 1 {
		t.Fatalf("sched ListUsers = %d users, %v; want exactly its own one", len(su), err)
	}

	// Where everything landed: each store's tables in its schema, nothing in
	// public, and each schema_migrations in the right shape.
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
	if count(`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'sched' AND table_name = 'schema_migrations' AND column_name = 'dirty'`) != 1 {
		t.Error("sched.schema_migrations is not the sched runner's table")
	}
	if count(`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'chat' AND table_name = 'schema_migrations' AND column_name = 'applied_at'`) != 1 {
		t.Error("chat.schema_migrations is not the chat runner's table")
	}

	// Reopening runs no migration and keeps the data.
	_ = chat.Close()
	_ = sched.Close()
	chat, sched = open()
	defer chat.Close()
	defer sched.Close()
	if rep, err := chat.MigrationStatus(ctx); err != nil || len(rep.Pending) != 0 {
		t.Errorf("chat migrations pending after reopen: %v, %v", rep.Pending, err)
	}
	if rep, err := scheddb.MigrationStatus(ctx, sched.Conn()); err != nil || len(rep.Pending) != 0 || rep.Dirty {
		t.Errorf("sched migrations pending/dirty after reopen: %+v, %v", rep, err)
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
	legacy, err := store.Open(dsn, store.DefaultPoolConfig()) // a legacy chat database: tables in public
	if err != nil {
		t.Fatalf("open legacy chat store: %v", err)
	}
	_ = legacy.Close()

	_, err = store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
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
