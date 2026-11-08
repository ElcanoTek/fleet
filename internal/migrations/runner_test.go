package migrations

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// TestLoadEmbedded: the shipped set is legacy chat 1–99 then legacy sched
// 101–177, in that order, every sched version with its down file, nothing at
// 200 or above yet.
func TestLoadEmbedded(t *testing.T) {
	t.Parallel()
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	for v := 1; v <= 99; v++ {
		want = append(want, v)
	}
	for v := 101; v <= LegacyLatest; v++ {
		want = append(want, v)
	}
	if got := versionsOf(set); !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v\nwant %v", got, want)
	}
	for _, m := range set.list {
		switch {
		case m.Version < SchedOffset:
			if m.Stream != StreamChat || m.Down != "" || !strings.HasPrefix(m.Name, "chat/") || m.SearchPath() != "chat" {
				t.Errorf("chat %d: %+v", m.Version, m.Name)
			}
		default:
			if m.Stream != StreamSched || m.Down == "" || !strings.HasPrefix(m.Name, "sched/") || m.SearchPath() != "sched" {
				t.Errorf("sched %d: %s (down %d bytes)", m.Version, m.Name, len(m.Down))
			}
		}
		if len(m.Checksum) != 64 {
			t.Errorf("%s: checksum %q", m.Name, m.Checksum)
		}
	}
	if m, _ := set.Get(177); m.Name != "sched/077_move_chat_schedules" || m.LegacyVersion() != 77 || m.LegacyName() != "077_move_chat_schedules" {
		t.Errorf("177 = %+v", m.Name)
	}
	if body, ok := Body(99); !ok || !strings.Contains(body, "CREATE SCHEMA IF NOT EXISTS core") {
		t.Errorf("Body(99) = %v", ok)
	}
}

// TestLoadRefuses: the loader refuses a build it could not run safely.
func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"sql/legacy/chat/001_a.sql":       {Data: []byte("SELECT 1;")},
			"sql/legacy/sched/001_a.up.sql":   {Data: []byte("SELECT 1;")},
			"sql/legacy/sched/001_a.down.sql": {Data: []byte("SELECT 1;")},
			"sql/README.md":                   {Data: []byte("not a migration")},
		}
	}
	if _, err := Load(base()); err != nil {
		t.Fatalf("a valid tree: %v", err)
	}
	for name, c := range map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"missing down":            {fstest.MapFS{"sql/0200_x.up.sql": {Data: []byte("SELECT 1;")}}, "down half"},
		"missing up":              {fstest.MapFS{"sql/0200_x.down.sql": {Data: []byte("SELECT 1;")}}, "no (or an empty)"},
		"below 200":               {fstest.MapFS{"sql/0199_x.up.sql": {Data: []byte("SELECT 1;")}, "sql/0199_x.down.sql": {Data: []byte("SELECT 1;")}}, "start at version 200"},
		"bad name":                {fstest.MapFS{"sql/200_x.up.sql": {Data: []byte("SELECT 1;")}}, "not a migration name"},
		"upper case":              {fstest.MapFS{"sql/0200_X.up.sql": {Data: []byte("SELECT 1;")}}, "not a migration name"},
		"two names":               {fstest.MapFS{"sql/0200_x.up.sql": {Data: []byte("SELECT 1;")}, "sql/0200_y.down.sql": {Data: []byte("SELECT 1;")}}, "two names"},
		"no-transaction":          {fstest.MapFS{"sql/0200_x.up.sql": {Data: []byte("-- fleet:no-transaction\nCREATE INDEX CONCURRENTLY i ON core.t (a);")}, "sql/0200_x.down.sql": {Data: []byte("SELECT 1;")}}, "no-transaction"},
		"subdirectory":            {fstest.MapFS{"sql/next/0200_x.up.sql": {Data: []byte("SELECT 1;")}}, "unexpected directory"},
		"legacy chat 100":         {fstest.MapFS{"sql/legacy/chat/100_x.sql": {Data: []byte("SELECT 1;")}}, "outside 1–99"},
		"legacy sched up missing": {fstest.MapFS{"sql/legacy/sched/002_b.down.sql": {Data: []byte("SELECT 1;")}}, "has no .up.sql"},
	} {
		fsys := base()
		for k, v := range c.files {
			fsys[k] = v
		}
		if _, err := Load(fsys); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want an error containing %q", name, err, c.want)
		}
	}
}

// orderTree is a toy sequence that logs each migration it runs, with the
// search_path it ran under, into core.order_log.
func orderTree() fstest.MapFS {
	logSQL := func(tag string) []byte {
		return []byte(`INSERT INTO core.order_log (tag) VALUES ('` + tag + `:' || coalesce(current_schema(), 'none'));`)
	}
	return fstest.MapFS{
		"sql/legacy/chat/001_log.sql":     {Data: []byte(`CREATE TABLE core.order_log (seq serial PRIMARY KEY, tag text NOT NULL); CREATE TABLE in_chat (i int); ` + string(logSQL("chat1")))},
		"sql/legacy/chat/002_b.sql":       {Data: logSQL("chat2")},
		"sql/legacy/sched/001_a.up.sql":   {Data: append([]byte(`CREATE TABLE in_sched (i int); `), logSQL("sched1")...)},
		"sql/legacy/sched/001_a.down.sql": {Data: []byte(`SELECT 1;`)},
		"sql/legacy/sched/002_b.up.sql":   {Data: logSQL("sched2")},
		"sql/legacy/sched/002_b.down.sql": {Data: []byte(`SELECT 1;`)},
		"sql/0200_new.up.sql":             {Data: logSQL("new200")},
		"sql/0200_new.down.sql":           {Data: []byte(`DELETE FROM core.order_log WHERE tag LIKE 'new200%';`)},
		"sql/0300_later.up.sql":           {Data: logSQL("new300")},
		"sql/0300_later.down.sql":         {Data: []byte(`DELETE FROM core.order_log WHERE tag LIKE 'new300%';`)},
	}
}

// TestUpMergedStreamOrder: one sequence, all of chat, then all of sched,
// then the new layout; each legacy file under its store's schema, each new
// one under pg_catalog; one ledger row per version.
func TestUpMergedStreamOrder(t *testing.T) {
	t.Parallel()
	db := emptyDB(t)
	r := runnerFor(t, orderTree())
	if err := r.up(context.Background(), db); err != nil {
		t.Fatalf("up: %v", err)
	}
	rows, err := db.Query(`SELECT tag FROM core.order_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	_ = rows.Close()
	want := []string{"chat1:chat", "chat2:chat", "sched1:sched", "sched2:sched", "new200:pg_catalog", "new300:pg_catalog"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if got := ledgerVersions(t, db); !reflect.DeepEqual(got, []int{1, 2, 101, 102, 200, 300}) {
		t.Fatalf("ledger = %v", got)
	}
	var inChat, inSched bool
	if err := db.QueryRow(`SELECT to_regclass('chat.in_chat') IS NOT NULL, to_regclass('sched.in_sched') IS NOT NULL`).Scan(&inChat, &inSched); err != nil || !inChat || !inSched {
		t.Fatalf("legacy objects not in their schemas: chat %v sched %v err %v", inChat, inSched, err)
	}
	// Current: a second run is the fast path and changes nothing.
	if !r.current(context.Background(), db) {
		t.Fatal("a migrated database must read as current")
	}
	if err := r.up(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

// TestUpRefusesUnknownVersion: a version this build does not ship (a newer
// fleet migrated the database) is refused, not ignored.
func TestUpRefusesUnknownVersion(t *testing.T) {
	t.Parallel()
	db := emptyDB(t)
	r := runnerFor(t, orderTree())
	ctx := context.Background()
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO core.schema_versions (version, name, checksum) VALUES (400, '0400_from_the_future', '')`)
	if r.current(ctx, db) {
		t.Fatal("an unknown version must not read as current")
	}
	var uv UnknownVersionError
	if err := r.up(ctx, db); !errors.As(err, &uv) || !reflect.DeepEqual(uv.Versions, []int{400}) {
		t.Fatalf("up = %v, want UnknownVersionError{400}", err)
	}
	// Status still lists it under Applied, so the downgrade is visible.
	rep, err := r.status(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep.Applied); n == 0 || rep.Applied[n-1].Version != 400 || *rep.CurrentVersion != 400 ||
		len(rep.Unknown) != 1 || rep.Unknown[0].Version != 400 || !rep.Refused() {
		t.Fatalf("status = %+v", rep)
	}
}

// TestUpFailedMigrationRecordsNothing: a migration that fails rolls back
// whole — no objects, no ledger row — and the ones before it stay applied.
func TestUpFailedMigrationRecordsNothing(t *testing.T) {
	t.Parallel()
	db := emptyDB(t)
	r := runnerFor(t, embeddedPlus(t, "fixture", "failing"))
	err := r.up(context.Background(), db)
	if err == nil || !strings.Contains(err.Error(), "migration 201 (0201_test_fails) failed") || !strings.Contains(err.Error(), "column \"no_such_column\" does not exist (line 3") {
		t.Fatalf("up = %v, want 0201's failure with its line", err)
	}
	got := ledgerVersions(t, db)
	if got[len(got)-1] != 200 {
		t.Fatalf("ledger ends at %d, want 200 (0201 recorded nothing)", got[len(got)-1])
	}
	var half bool
	if err := db.QueryRow(`SELECT to_regclass('core.m6_half_done') IS NOT NULL`).Scan(&half); err != nil || half {
		t.Fatalf("0201's table survived its failure (%v, %v)", half, err)
	}
}

// TestUpConcurrentPools: two pools (two processes) running Up on one fresh
// database at once both succeed, and every version is applied once.
func TestUpConcurrentPools(t *testing.T) {
	t.Parallel()
	a, dsn := emptyDBDSN(t)
	b := openPool(t, dsn)
	r := testEmbeddedRunner(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, db := range []*sql.DB{a, b} {
		wg.Add(1)
		go func(i int, db *sql.DB) {
			defer wg.Done()
			errs[i] = r.up(context.Background(), db)
		}(i, db)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Up #%d: %v", i, err)
		}
	}
	if got := ledgerVersions(t, a); !reflect.DeepEqual(got, versionsOf(r.set)) {
		t.Fatalf("ledger = %v", got)
	}
}

// TestUpWaitsForTheLegacyLocks: a pre-M6 binary holding either legacy
// runner's lock keeps this runner from adopting until it lets go; the wait is
// bounded.
func TestUpWaitsForTheLegacyLocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	legacyBuild(t, db, 99, 77)
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key  int64
		what string
	}{
		{legacyChatLockKey, "legacy chat migration lock"},
		{legacySchedLockKey(name), "legacy sched migration lock"},
		{LockKey, "migration lock"},
	} {
		holder, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var held bool
		if err := holder.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, c.key).Scan(&held); err != nil || !held {
			t.Fatalf("hold %s: %v %v", c.what, held, err)
		}
		r := testEmbeddedRunner(t)
		r.lockTimeout = 300 * time.Millisecond
		start := time.Now()
		err = r.up(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "waiting for the "+c.what) {
			t.Fatalf("up while another session holds the %s = %v", c.what, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("lock wait took %s", d)
		}
		if hasLedger(t, db) {
			t.Fatalf("adopted while the %s was held", c.what)
		}
		_, _ = holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, c.key)
		_ = holder.Close()
	}
	if err := testEmbeddedRunner(t).up(ctx, db); err != nil {
		t.Fatalf("up once the locks are free: %v", err)
	}
	if !hasLedger(t, db) {
		t.Fatal("not adopted")
	}
}

// TestGolangMigrateLockID: the legacy sched key is exactly the key
// golang-migrate v4.20.1's database.GenerateAdvisoryLockId(db, schema, table)
// returned (computed with the library before it was removed).
func TestGolangMigrateLockID(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		db, schema, table string
		want              int64
	}{
		{"fleet_sched_test", "public", "schema_migrations", 3725944996},
		{"sched", "public", "schema_migrations", 3410033506},
		{"fleet", "public", "schema_migrations", 3906235543},
		{"orchestrator", "app", "schema_migrations", 2277950650},
	} {
		if got := golangMigrateLockID(c.db, c.schema, c.table); got != c.want {
			t.Errorf("golangMigrateLockID(%q, %q, %q) = %d, want %d", c.db, c.schema, c.table, got, c.want)
		}
	}
	if legacySchedLockKey("fleet") != golangMigrateLockID("fleet", "sched", "schema_migrations") {
		t.Error("legacySchedLockKey is not the key for schema sched")
	}
}

func TestLineColumn(t *testing.T) {
	t.Parallel()
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

// TestDirtyAndForce: a dirty row is refused at boot; Force records exactly
// the versions up to its argument, clean, running nothing.
func TestDirtyAndForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	r := runnerFor(t, orderTree())
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE core.schema_versions SET dirty = true WHERE version = 300`)
	var de DirtyError
	if err := r.up(ctx, db); !errors.As(err, &de) || !reflect.DeepEqual(de.Versions, []int{300}) {
		t.Fatalf("up = %v, want DirtyError{300}", err)
	}
	if err := r.force(ctx, db, 150); err == nil {
		t.Fatal("force to a version the build does not ship must fail")
	}
	if err := r.force(ctx, db, 101); err != nil {
		t.Fatalf("force 101: %v", err)
	}
	if got := ledgerVersions(t, db); !reflect.DeepEqual(got, []int{1, 2, 101}) {
		t.Fatalf("after force 101 the ledger = %v", got)
	}
	mustExec(t, db, `DELETE FROM core.schema_versions WHERE version = 2`)
	if err := r.force(ctx, db, 200); err != nil {
		t.Fatal(err)
	}
	if got := ledgerVersions(t, db); !reflect.DeepEqual(got, []int{1, 2, 101, 102, 200}) {
		t.Fatalf("after force 200 the ledger = %v", got)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM core.order_log`).Scan(&n); err != nil || n != 6 {
		t.Fatalf("force ran migrations: %d log rows (%v)", n, err)
	}
	if err := r.force(ctx, emptyDB(t), 200); !errors.Is(err, ErrNotAdopted) {
		t.Fatalf("force on an unadopted database = %v", err)
	}
}

// TestDownRoundTrip: the test-only 0200 goes down and up again; the legacy
// migrations never go down.
func TestDownRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	r := runnerFor(t, embeddedPlus(t, "fixture"))
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	fixture := func() (table, column bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT to_regclass('core.m6_fixture') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'sched' AND table_name = 'tasks' AND column_name = 'm6_fixture_note')`).Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		return table, column
	}
	if tb, col := fixture(); !tb || !col {
		t.Fatalf("0200 not applied: table %v column %v", tb, col)
	}

	for _, to := range []int{176, 100, 0} {
		if _, err := r.down(ctx, db, to, false); err == nil || !strings.Contains(err.Error(), "lowest target is 177") {
			t.Fatalf("down --to %d = %v, want the legacy floor refusal", to, err)
		}
	}
	plan, err := r.down(ctx, db, LegacyLatest, true)
	if err != nil || len(plan.Steps) != 1 || plan.Steps[0].Version != 200 {
		t.Fatalf("dry run = %+v, %v", plan, err)
	}
	if tb, _ := fixture(); !tb {
		t.Fatal("a dry run changed the database")
	}
	if _, err := r.down(ctx, db, LegacyLatest, false); err != nil {
		t.Fatalf("down: %v", err)
	}
	if tb, col := fixture(); tb || col {
		t.Fatalf("0200 not undone: table %v column %v", tb, col)
	}
	if got := ledgerVersions(t, db); got[len(got)-1] != LegacyLatest {
		t.Fatalf("ledger ends at %d after down", got[len(got)-1])
	}
	if plan, err := r.down(ctx, db, LegacyLatest, false); err != nil || len(plan.Steps) != 0 {
		t.Fatalf("down again = %+v, %v; want nothing to do", plan, err)
	}
	if err := r.up(ctx, db); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if tb, col := fixture(); !tb || !col {
		t.Fatal("0200 not re-applied")
	}
	var note string
	if err := db.QueryRow(`SELECT note FROM core.m6_fixture WHERE id = 1`).Scan(&note); err != nil || note != "seeded by 0200" {
		t.Fatalf("re-applied 0200's row = %q, %v", note, err)
	}
}

// TestSentinels: the first migration at or above 200 marks both legacy
// ledgers so a pre-M6 binary refuses the database, on an adopted database
// and on a fresh one alike; a legacy migration does not.
func TestSentinels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	adopted := emptyDB(t)
	legacyBuild(t, adopted, 99, 77)
	if err := testEmbeddedRunner(t).up(ctx, adopted); err != nil {
		t.Fatal(err)
	}
	chat, sched := legacyLedgers(t, adopted)
	if len(chat) != 99 || !reflect.DeepEqual(sched, []string{"(77,f)"}) {
		t.Fatalf("legacy ledgers before 0200: chat %d rows, sched %v", len(chat), sched)
	}
	fresh := emptyDB(t)
	r := runnerFor(t, embeddedPlus(t, "fixture"))
	for name, db := range map[string]*sql.DB{"adopted": adopted, "fresh": fresh} {
		if err := r.up(ctx, db); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var chatMax int
		var sentinel string
		if err := db.QueryRow(`SELECT max(version) FROM chat.schema_migrations`).Scan(&chatMax); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT name FROM chat.schema_migrations WHERE version = 100`).Scan(&sentinel); err != nil || sentinel != ChatSentinelName {
			t.Fatalf("%s: chat sentinel = %q, %v", name, sentinel, err)
		}
		// The pre-M6 chat runner refused a database whose highest version
		// was newer than its own latest (99); its sched runner refused a
		// version it had no file for.
		if chatMax <= 99 {
			t.Fatalf("%s: chat ledger max %d would not stop a pre-M6 binary", name, chatMax)
		}
		_, sched := legacyLedgers(t, db)
		if !reflect.DeepEqual(sched, []string{"(178,f)"}) {
			t.Fatalf("%s: sched ledger = %v, want the single row 178", name, sched)
		}
	}
	// Re-adopting such a database (its ledger dropped) is refused: the
	// sentinels are versions this build does not ship.
	mustExec(t, adopted, `DROP TABLE core.schema_versions`)
	var ae AdoptionError
	if err := r.up(ctx, adopted); !errors.As(err, &ae) {
		t.Fatalf("re-adoption past the sentinels = %v", err)
	}
}

// TestUpRefusesAnEditedMigration: a ledger row whose checksum (or name) is
// not the build's file of that version — a shipped migration edited after
// the database ran it — is refused, on the fast path too; legacy rows are
// verified like the rest. `force` records the build's files again.
func TestUpRefusesAnEditedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	r := runnerFor(t, embeddedPlus(t, "fixture"))
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version int
		set     string
	}{
		{200, `checksum = 'edited'`},
		{7, `checksum = md5('x')`},
		{150, `name = 'sched/050_renamed'`},
	} {
		mustExec(t, db, `UPDATE core.schema_versions SET `+c.set+` WHERE version = $1`, c.version)
	}
	if r.current(ctx, db) {
		t.Fatal("a ledger with edited rows read as current")
	}
	var ce ChecksumError
	if err := r.up(ctx, db); !errors.As(err, &ce) || len(ce.Rows) != 3 || ce.Rows[0].Version != 7 || ce.Rows[2].Version != 200 ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("up = %v, want a ChecksumError naming 7, 150 and 200", err)
	}
	// Status reports what Up refuses (Codex 4506166386).
	rep, err := r.status(ctx, db)
	if err != nil || !rep.Refused() || len(rep.Mismatched) != 3 || rep.Mismatched[0].Version != 7 || len(rep.Unknown) != 0 {
		t.Fatalf("status over an edited ledger = %+v, %v", rep.Mismatched, err)
	}
	if _, err := r.down(ctx, db, LegacyLatest, true); !errors.As(err, &ce) {
		t.Fatalf("down over an edited ledger = %v, want the same refusal", err)
	}
	if err := r.force(ctx, db, 200); err != nil {
		t.Fatal(err)
	}
	if !r.current(ctx, db) {
		t.Fatal("force did not record the build's files")
	}
}
