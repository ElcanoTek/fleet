package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var selfMigrations atomic.Int32

// selfSpec is a tiny template: one table with one row.
var selfSpec = Spec{
	Name:        "selftest",
	Fingerprint: func() string { return "selftest-v1" },
	Migrate: func(ctx context.Context, dsn string) error {
		selfMigrations.Add(1)
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.ExecContext(ctx, `CREATE TABLE seeded (v text); INSERT INTO seeded VALUES ('from-template')`)
		return err
	},
}

func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func exists(t *testing.T, name string) bool {
	t.Helper()
	var n int
	if err := get(t).admin.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func dbName(dsn string) string {
	_, rest, _ := strings.Cut(dsn, "@")
	_, rest, _ = strings.Cut(rest, "/")
	name, _, _ := strings.Cut(rest, "?")
	return name
}

// TestNewClonesAreIndependent: every New is its own database holding the
// template's data, and a write to one is invisible to another.
func TestNewClonesAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := open(t, New(t, selfSpec)), open(t, New(t, selfSpec))
	if _, err := a.Exec(`INSERT INTO seeded VALUES ('only-in-a')`); err != nil {
		t.Fatal(err)
	}
	for db, want := range map[*sql.DB]int{a: 2, b: 1} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM seeded`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("rows = %d, want %d", n, want)
		}
	}
}

// TestTemplateBuiltOncePerProcess: a second New reuses the template (no
// second Migrate), and the template is marked so nobody can connect to it.
func TestTemplateBuiltOncePerProcess(t *testing.T) {
	t.Parallel()
	New(t, selfSpec)
	before := selfMigrations.Load()
	New(t, selfSpec)
	if after := selfMigrations.Load(); after != before {
		t.Errorf("Migrate ran again (%d → %d)", before, after)
	}
	var tmpl, conn bool
	err := get(t).admin.QueryRow(`SELECT datistemplate, datallowconn FROM pg_database
		WHERE datname LIKE 'fleet\_tmpl\_selftest\_%' ORDER BY oid DESC LIMIT 1`).Scan(&tmpl, &conn)
	if err != nil {
		t.Fatal(err)
	}
	if !tmpl || conn {
		t.Errorf("template datistemplate=%v datallowconn=%v, want true/false", tmpl, conn)
	}
}

// TestCloneDroppedAtCleanup: the database is gone once the test ends, even
// with a connection still open on it (a goroutine that outlived its test).
func TestCloneDroppedAtCleanup(t *testing.T) {
	t.Parallel()
	var name string
	var leaked *sql.DB
	t.Run("owner", func(t *testing.T) {
		dsn := Empty(t)
		name = dbName(dsn)
		leaked, _ = sql.Open("pgx", dsn)
		if err := leaked.Ping(); err != nil {
			t.Fatal(err)
		}
	})
	defer func() { _ = leaked.Close() }()
	if exists(t, name) {
		t.Errorf("%s still exists after its test ended", name)
	}
}

// TestEmptyAndLegacyPairAreEmpty: no tables in either.
func TestEmptyAndLegacyPairAreEmpty(t *testing.T) {
	t.Parallel()
	chat, sched := LegacyPair(t)
	if chat == sched {
		t.Fatal("LegacyPair returned one database twice")
	}
	for _, dsn := range []string{Empty(t), chat, sched} {
		var n int
		if err := open(t, dsn).QueryRow(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d tables, want none", dbName(dsn), n)
		}
	}
}

// TestCollectDropsOnlyDeadOwners: garbage collection drops the per-test
// databases of a process that is gone (its owner lock is free) and leaves a
// live process's alone, and never touches a database outside its prefixes.
func TestCollectDropsOnlyDeadOwners(t *testing.T) {
	t.Parallel()
	s := get(t)
	ctx := t.Context()
	const live, dead = "0badc0de", "0deadbee"
	// A "live" owner: its lock is held on a connection of its own.
	holder, err := s.admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1::int4, $2::int4)`, lockNSOwner, int32(0x0badc0de)); err != nil {
		t.Fatal(err)
	}
	liveDB, deadDB := clonePrefix+live+"_1", clonePrefix+dead+"_1"
	other := "fleetx_t_" + dead + "_1" // outside the prefixes
	for _, n := range []string{liveDB, deadDB, other} {
		if _, err := s.admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(n)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{liveDB, deadDB, other} {
			_, _ = s.admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(n) + ` WITH (FORCE)`)
		}
	})
	if err := s.withBuildLock(ctx, s.collect); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]bool{liveDB: true, deadDB: false, other: true} {
		if got := exists(t, n); got != want {
			t.Errorf("%s exists = %v, want %v", n, got, want)
		}
	}
	// This process's own databases survive too (its lock is held).
	mine := dbName(Empty(t))
	if err := s.withBuildLock(ctx, s.collect); err != nil {
		t.Fatal(err)
	}
	if !exists(t, mine) {
		t.Errorf("collect dropped this live process's own %s", mine)
	}
}

// TestDropRefusesForeignNames: drop is confined to testdb's prefixes.
func TestDropRefusesForeignNames(t *testing.T) {
	t.Parallel()
	s := get(t)
	for _, n := range []string{"postgres", "chat_r5", "fleet_r5", "fleet_test", "fleet"} {
		if err := s.drop(t.Context(), n); err == nil {
			t.Errorf("drop(%q) did not refuse", n)
		}
	}
}

// TestSpecValidation: a bad Spec fails loudly.
func TestSpecValidation(t *testing.T) {
	t.Parallel()
	s := get(t)
	for _, sp := range []Spec{
		{Name: "Bad-Name", Fingerprint: selfSpec.Fingerprint, Migrate: selfSpec.Migrate},
		{Name: "ok"},
	} {
		if _, err := s.template(t.Context(), sp); err == nil {
			t.Errorf("template(%+v) accepted", fmt.Sprint(sp.Name))
		}
	}
}

// TestBackgroundModeLeavesNothingBehind drives Run's background mode on a
// server of its own (its own owner id): clones made ahead are handed out,
// finished tests' databases are dropped off the critical path, and shutdown
// drops the unused ones and waits — so nothing of that owner remains.
func TestBackgroundModeLeavesNothingBehind(t *testing.T) {
	main := get(t)
	runMode.Store(true)
	s, err := connect(main.base.String())
	runMode.Store(false)
	live.Store(main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.admin.Close() }()
	if !s.bg.on.Load() {
		t.Fatal("background mode is off under Run")
	}
	tmpl, err := s.template(t.Context(), selfSpec)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, 6)
	ahead := 0
	for range 6 {
		name, ok := s.pooled(tmpl)
		if ok {
			ahead++
		} else {
			name = s.newName()
			if err := s.createDB(t.Context(), name, tmpl); err != nil {
				t.Fatal(err)
			}
		}
		got = append(got, name)
		time.Sleep(300 * time.Millisecond) // let the filler work
	}
	if ahead == 0 {
		t.Error("no test got a clone made ahead")
	}
	for _, name := range got {
		s.dropLater(name)
	}
	s.shutdown()
	var n int
	if err := main.admin.QueryRow(`SELECT count(*) FROM pg_database WHERE starts_with(datname, $1)`,
		clonePrefix+s.ownerHex()+"_").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d database(s) of the background server left behind", n)
	}
}
