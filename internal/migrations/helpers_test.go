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
