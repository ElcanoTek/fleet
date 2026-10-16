package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestAvailableMigrations checks the embedded up-migration enumerator: strictly
// ascending, positive versions, and names with the .up.sql suffix stripped. Pure
// (no DB), so it always runs.
func TestAvailableMigrations(t *testing.T) {
	avail, err := availableMigrations()
	if err != nil {
		t.Fatalf("availableMigrations: %v", err)
	}
	if len(avail) == 0 {
		t.Fatal("expected embedded up-migrations, got none")
	}
	for i, m := range avail {
		if m.Version <= 0 {
			t.Errorf("non-positive version %d for %q", m.Version, m.Name)
		}
		if strings.Contains(m.Name, ".") {
			t.Errorf("name %q should have the .up.sql suffix stripped", m.Name)
		}
		if i > 0 && avail[i-1].Version >= m.Version {
			t.Errorf("availableMigrations not strictly ascending at index %d (%d then %d)", i, avail[i-1].Version, m.Version)
		}
	}
}

// TestMigrationStatus checks the applied-vs-pending report against a freshly
// migrated sched DB. Skips without DATABASE_URL (integration test).
func TestMigrationStatus(t *testing.T) {
	db := setupTestDB(t) // Init runs every migration

	rep, err := MigrationStatus(context.Background(), db.Conn())
	if err != nil {
		t.Fatalf("MigrationStatus: %v", err)
	}
	if rep.DB != "sched" || rep.Runner != "golang-migrate" || rep.MigrationTable != "schema_migrations" {
		t.Fatalf("unexpected metadata: %+v", rep)
	}
	if rep.Dirty {
		t.Errorf("a freshly migrated DB should not be dirty")
	}
	avail, err := availableMigrations()
	if err != nil {
		t.Fatalf("availableMigrations: %v", err)
	}
	if rep.CurrentVersion == nil {
		t.Fatal("current version should be set after migrations run")
	}
	if latest := avail[len(avail)-1].Version; *rep.CurrentVersion != latest {
		t.Errorf("current version = %d, want the latest embedded version %d", *rep.CurrentVersion, latest)
	}
	if len(rep.Pending) != 0 {
		t.Errorf("a fully-migrated DB should report 0 pending, got %d: %+v", len(rep.Pending), rep.Pending)
	}
	if len(rep.Applied) != len(avail) {
		t.Errorf("applied=%d, want=%d (every embedded up-migration)", len(rep.Applied), len(avail))
	}
}

// TestAdvisoryLockIDMatchesGolangMigrate pins the lock key to the values
// golang-migrate v4.20.1's database.GenerateAdvisoryLockId(db, schema, table)
// returned (computed with the library before it was removed). A different key
// would let an old and a new fleet migrate the same database at once during a
// rolling restart.
func TestAdvisoryLockIDMatchesGolangMigrate(t *testing.T) {
	for _, c := range []struct {
		db, schema, table string
		want              int64
	}{
		{"fleet_sched_test", "public", "schema_migrations", 3725944996},
		{"sched", "public", "schema_migrations", 3410033506},
		{"fleet", "public", "schema_migrations", 3906235543},
		{"orchestrator", "app", "schema_migrations", 2277950650},
	} {
		if got := advisoryLockID(c.db, c.schema, c.table); got != c.want {
			t.Errorf("advisoryLockID(%q, %q, %q) = %d, want %d", c.db, c.schema, c.table, got, c.want)
		}
	}
}

// TestLoadMigrations: ascending versions, names, a down-only version kept with
// no body (golang-migrate recorded it without running anything), files that
// do not match the pattern ignored, a duplicate version an error.
func TestLoadMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"m/010_ten.up.sql":     {Data: []byte("SELECT 10;")},
		"m/010_ten.down.sql":   {Data: []byte("SELECT -10;")},
		"m/2_two.up.sql":       {Data: []byte("SELECT 2;")},
		"m/003_three.down.sql": {Data: []byte("SELECT -3;")},
		"m/README.md":          {Data: []byte("not a migration")},
	}
	migs, err := loadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	want := []schedMigration{
		{Version: 2, Name: "2_two", Up: "SELECT 2;"},
		{Version: 3, Name: "003_three"},
		{Version: 10, Name: "010_ten", Up: "SELECT 10;"},
	}
	if !reflect.DeepEqual(migs, want) {
		t.Fatalf("loadMigrations = %+v, want %+v", migs, want)
	}
	over := fstest.MapFS{"m/99999999999999999999_huge.up.sql": {Data: []byte("SELECT 1;")}}
	if _, err := loadMigrations(over, "m"); err == nil || !strings.Contains(err.Error(), "version out of range") {
		t.Fatalf("over-range version: err = %v", err)
	}
	fsys["m/10_ten_again.up.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	if _, err := loadMigrations(fsys, "m"); err == nil || !strings.Contains(err.Error(), "duplicate migration file") {
		t.Fatalf("duplicate version: err = %v", err)
	}
}

func TestLineColumn(t *testing.T) {
	for _, c := range []struct {
		s              string
		pos, line, col int
		ok             bool
	}{
		{"SELECT 1;\nSELEC 2;", 11, 2, 1, true},
		{"ab\r\ncd", 4, 2, 1, true},
		{"abc", 4, 0, 0, false},
		{"abc", 0, 0, 0, false},
	} {
		line, col, ok := lineColumn(c.s, c.pos)
		if line != c.line || col != c.col || ok != c.ok {
			t.Errorf("lineColumn(%q, %d) = %d, %d, %v; want %d, %d, %v", c.s, c.pos, line, col, ok, c.line, c.col, c.ok)
		}
	}
}

// migrationSchemaDB opens a pool whose every connection uses a fresh schema,
// so the runner's tracking table, lock key and test tables are its own and the
// shared sched test database is not touched. Skips without DATABASE_URL.
func migrationSchemaDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping integration tests")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Database unavailable, skipping integration tests: %v", err)
		}
		t.Fatal(err)
	}
	schema := fmt.Sprintf("migtest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, schema
}

func trackingRow(t *testing.T, db *sql.DB) (int64, bool, bool) {
	t.Helper()
	var v int64
	var dirty bool
	err := db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, dirty, true
}

var testMigrations = fstest.MapFS{
	"m/001_a.up.sql":   {Data: []byte("CREATE TABLE a (id int);")},
	"m/001_a.down.sql": {Data: []byte("DROP TABLE a;")},
	"m/002_b.up.sql":   {Data: []byte("CREATE TABLE b (id int);\nINSERT INTO b VALUES (1);")},
	"m/002_b.down.sql": {Data: []byte("DROP TABLE b;")},
	"m/003_c.up.sql":   {Data: []byte("ALTER TABLE b ADD COLUMN note text;")},
	"m/003_c.down.sql": {Data: []byte("ALTER TABLE b DROP COLUMN note;")},
}

func TestMigrateUp_FreshThenNoChange(t *testing.T) {
	db, _ := migrationSchemaDB(t)
	ctx := context.Background()
	if err := migrateUp(ctx, db, testMigrations, "m"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if v, dirty, ok := trackingRow(t, db); !ok || v != 3 || dirty {
		t.Fatalf("tracking row = %d, %v, %v; want 3, clean", v, dirty, ok)
	}
	if _, err := db.Exec(`SELECT note FROM b`); err != nil {
		t.Fatalf("migration 3 not applied: %v", err)
	}
	if err := migrateUp(ctx, db, testMigrations, "m"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("second run: err = %v, want ErrNoChange", err)
	}
}

// TestMigrateUp_ContinuesGolangMigrateState: a database left by golang-migrate
// (its own DDL for the tracking table, one clean row) continues from the next
// version, and the table keeps exactly that shape.
func TestMigrateUp_ContinuesGolangMigrateState(t *testing.T) {
	db, schema := migrationSchemaDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS "` + schema + `"."schema_migrations" (version bigint not null primary key, dirty boolean not null)`,
		`INSERT INTO "` + schema + `"."schema_migrations" (version, dirty) VALUES (2, false)`,
		`CREATE TABLE a (id int)`,
		`CREATE TABLE b (id int)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateUp(ctx, db, testMigrations, "m"); err != nil {
		t.Fatalf("migrateUp: %v", err)
	}
	if v, dirty, _ := trackingRow(t, db); v != 3 || dirty {
		t.Fatalf("tracking row = %d, %v; want 3, clean", v, dirty)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM b`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("migration 2 re-ran (rows in b = %d, err %v)", n, err)
	}
	var cols []string
	rows, err := db.Query(`SELECT column_name || ':' || data_type || ':' || is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'schema_migrations' ORDER BY ordinal_position`, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if want := []string{"version:bigint:NO", "dirty:boolean:NO"}; !reflect.DeepEqual(cols, want) {
		t.Fatalf("schema_migrations columns = %v, want %v", cols, want)
	}
}

// TestMigrateUp_FailureLeavesDirtyThenForce: a failing migration leaves the row
// dirty at its version; every later run refuses with DirtyError; forcing the
// last good version lets the (fixed) set continue.
func TestMigrateUp_FailureLeavesDirtyThenForce(t *testing.T) {
	db, _ := migrationSchemaDB(t)
	ctx := context.Background()
	broken := fstest.MapFS{
		"m/001_a.up.sql": testMigrations["m/001_a.up.sql"],
		// One Exec, two statements: the first must not survive the second's
		// failure (a multi-statement simple query is one implicit transaction).
		"m/002_b.up.sql": {Data: []byte("CREATE TABLE b (id int);\nSELEC 1;")},
	}
	err := migrateUp(ctx, db, broken, "m")
	if err == nil || !strings.Contains(err.Error(), "migration 002_b failed") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("broken migration: err = %v", err)
	}
	if v, dirty, _ := trackingRow(t, db); v != 2 || !dirty {
		t.Fatalf("tracking row = %d, %v; want 2, dirty", v, dirty)
	}
	if _, err := db.Exec(`SELECT 1 FROM b`); err == nil {
		t.Fatal("first statement of the failed migration was committed")
	}
	var de DirtyError
	if err := migrateUp(ctx, db, testMigrations, "m"); !errors.As(err, &de) || de.Version != 2 {
		t.Fatalf("dirty DB: err = %v, want DirtyError{2}", err)
	}
	if err := forceVersion(ctx, db, testMigrations, "m", 7); err == nil {
		t.Fatal("forcing an unknown version should fail")
	}
	if err := forceVersion(ctx, db, testMigrations, "m", 1); err != nil {
		t.Fatalf("force 1: %v", err)
	}
	if err := migrateUp(ctx, db, testMigrations, "m"); err != nil {
		t.Fatalf("after force: %v", err)
	}
	if v, dirty, _ := trackingRow(t, db); v != 3 || dirty {
		t.Fatalf("tracking row = %d, %v; want 3, clean", v, dirty)
	}
	if err := forceVersion(ctx, db, testMigrations, "m", -1); err != nil {
		t.Fatalf("force -1: %v", err)
	}
	if _, _, ok := trackingRow(t, db); ok {
		t.Fatal("force -1 should leave the tracking table empty")
	}
}

func TestMigrateUp_DatabaseAheadOfBuildRefuses(t *testing.T) {
	db, _ := migrationSchemaDB(t)
	ctx := context.Background()
	if err := migrateUp(ctx, db, testMigrations, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := migrateUp(ctx, db, testMigrations, "m"); err == nil || !strings.Contains(err.Error(), "no migration found for version 99") {
		t.Fatalf("err = %v", err)
	}
}

// TestMigrateUp_WaitsForTheSharedLock: while another session holds
// golang-migrate's lock key for this database/schema, the runner does not
// proceed; it gives up at the lock timeout and succeeds once the lock is free.
func TestMigrateUp_WaitsForTheSharedLock(t *testing.T) {
	db, schema := migrationSchemaDB(t)
	ctx := context.Background()
	var dbName string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	key := advisoryLockID(dbName, schema, migrationsTable)
	// The decimal-string form golang-migrate sent, on purpose.
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, strconv.FormatInt(key, 10)); err != nil {
		t.Fatal(err)
	}
	old := migrationLockTimeout
	migrationLockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { migrationLockTimeout = old })
	if err := migrateUp(ctx, db, testMigrations, "m"); err == nil || !strings.Contains(err.Error(), "can't acquire database lock") {
		t.Fatalf("with the lock held: err = %v", err)
	}
	var tbl sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('schema_migrations')`).Scan(&tbl); err != nil || tbl.Valid {
		t.Fatalf("nothing should have been created while the lock was held (table %v, err %v)", tbl, err)
	}
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	if err := migrateUp(ctx, db, testMigrations, "m"); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}
