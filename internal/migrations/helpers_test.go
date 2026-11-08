package migrations

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }

// emptyDB opens a pool on a fresh, empty database of the test's own.
func emptyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := emptyDBDSN(t)
	return db
}

// emptyDBDSN is emptyDB and the database's DSN.
func emptyDBDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := testdb.Empty(t)
	return openPool(t, dsn), dsn
}

func openPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(testdb.PoolMaxOpen)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// embeddedPlus is the embedded tree with the files of testdata dir added
// under sql/ (test-only new-layout migrations, 0200 and up).
func embeddedPlus(t *testing.T, dirs ...string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(embedded, path)
		if err != nil {
			return err
		}
		out[path] = &fstest.MapFile{Data: body}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join("testdata", dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			body, err := os.ReadFile(filepath.Join("testdata", dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out["sql/"+e.Name()] = &fstest.MapFile{Data: body}
		}
	}
	return out
}

// runnerFor loads fsys into a runner with a short lock timeout.
func runnerFor(t *testing.T, fsys fs.FS) *runner {
	t.Helper()
	set, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := newRunner(set)
	r.lockTimeout = 20 * time.Second
	return r
}

func testEmbeddedRunner(t *testing.T) *runner {
	t.Helper()
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(set)
	r.lockTimeout = 20 * time.Second
	return r
}

// ledgerVersions reads core.schema_versions' versions, ascending.
func ledgerVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM core.schema_versions ORDER BY version`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func hasLedger(t *testing.T, db *sql.DB) bool {
	t.Helper()
	ok, err := ledgerExists(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func versionsOf(set *Set) []int {
	out := make([]int, 0, len(set.list))
	for _, m := range set.list {
		out = append(out, m.Version)
	}
	return out
}

func mustExec(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// legacyBuild builds a database the way a pre-M6 release did: schemas chat
// and sched, the legacy chat files through chatThrough recorded in
// chat.schema_migrations in the old runner's format (version, name, unix
// applied_at), then the legacy sched files through schedThrough recorded in
// golang-migrate's single {version, dirty} row — chat first, then sched, as
// every boot ran them.
func legacyBuild(t *testing.T, db *sql.DB, chatThrough, schedThrough int) {
	t.Helper()
	ctx := context.Background()
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE SCHEMA IF NOT EXISTS chat`)
	mustExec(t, db, `CREATE SCHEMA IF NOT EXISTS sched`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS chat.schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS sched.schema_migrations (version bigint not null primary key, dirty boolean not null)`)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	run := func(m Migration, record func(*sql.Tx) error) {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+m.Stream); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, m.Up); err != nil {
			t.Fatalf("legacy %s: %v", m.Name, err)
		}
		if err := record(tx); err != nil {
			t.Fatalf("record legacy %s: %v", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range set.list {
		if m.Stream != StreamChat || m.Version > chatThrough {
			continue
		}
		run(m, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO chat.schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
				m.Version, m.LegacyName(), int64(1700000000+m.Version))
			return err
		})
	}
	for _, m := range set.list {
		if m.Stream != StreamSched || m.LegacyVersion() > schedThrough {
			continue
		}
		run(m, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `TRUNCATE sched.schema_migrations`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO sched.schema_migrations (version, dirty) VALUES ($1, false)`, m.LegacyVersion())
			return err
		})
	}
}

// legacyLedgers snapshots both legacy ledgers as text.
func legacyLedgers(t *testing.T, db *sql.DB) (chat, sched []string) {
	t.Helper()
	read := func(q string) []string {
		var ok bool
		_ = db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, q).Scan(&ok)
		if !ok {
			return nil
		}
		rows, err := db.Query(`SELECT t::text FROM ` + q + ` t ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	return read("chat.schema_migrations"), read("sched.schema_migrations")
}
