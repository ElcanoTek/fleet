package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// spendRecords are the tables the usage report, the adoption report and the
// budgets read spend from: one row per task iteration (sched) and one per
// chat turn (chat). Each outlives what it describes — the task, the
// conversation, the account — so spend already incurred keeps counting after
// a person or a retention sweep deletes those (docs/USAGE-ANALYTICS.md).
//
// The checks below refuse anything that deletes spend rows. Two kinds of
// legitimate work go through them as follows:
//
//   - Erasing personal data from a spend record rewrites the identifying
//     column — turn_metrics.user_email, task_iterations.created_by — with an
//     UPDATE, which these checks allow, and keeps the row's meters.
//   - Pruning spend so old that no budget window or report range reaches it is
//     a reviewed exception: an entry in spendRecordDeleteAllowedFiles (or
//     spendRecordDeleteAllowedFunctions) with its written reason, and
//     docs/USAGE-ANALYTICS.md saying what is pruned and when.
var spendRecords = []string{"sched.task_iterations", "chat.turn_metrics"}

// spendRecordDeleteAllowedFiles maps a repo-relative path that may delete
// from a spend record to the reviewed reason it may. Empty: nothing may.
var spendRecordDeleteAllowedFiles = map[string]string{}

// spendRecordDeleteAllowedFunctions maps a database function
// ("<schema>.<name>") that may delete from a spend record to the reviewed
// reason it may. Empty: nothing may.
var spendRecordDeleteAllowedFunctions = map[string]string{}

// The pieces of spendRecordDelete: an identifier (optionally quoted), any
// table reference, and a reference to a spend record. The trailing \b keeps
// task_iterations_archive and the like out.
const (
	sqlIdent      = `"?\w+"?`
	sqlAnyTable   = `(?:only\s+)?(?:` + sqlIdent + `\.)?` + sqlIdent + `(?:\s*\*)?`
	sqlSpendTable = `(?:only\s+)?(?:` + sqlIdent + `\.)?"?(?:task_iterations|turn_metrics)\b"?`
)

// spendRecordDelete matches a statement that removes rows from a spend
// record: DELETE FROM one, or a TRUNCATE whose table list names one anywhere.
// It reads source text, so SQL assembled at run time from pieces
// ("DELETE FROM " + table) is not caught; the database-side check in
// TestSpendRecordsOutliveTheirParents does not depend on how SQL is written.
var spendRecordDelete = regexp.MustCompile(`(?is)\b(?:delete\s+from\s+` + sqlSpendTable +
	`|truncate\s+(?:table\s+)?(?:` + sqlAnyTable + `\s*,\s*)*` + sqlSpendTable + `)`)

// TestSpendRecordDeletePattern pins what the source scan treats as deleting
// spend rows, so the guard is known to fire on the forms it must catch.
func TestSpendRecordDeletePattern(t *testing.T) {
	t.Parallel()
	for _, stmt := range []string{
		`DELETE FROM task_iterations WHERE task_id = $1`,
		`delete from sched.task_iterations`,
		"DELETE\n\t FROM   chat.turn_metrics m WHERE m.completed_at < $1",
		`DELETE FROM ONLY "sched"."task_iterations"`,
		`DELETE FROM "turn_metrics"`,
		`TRUNCATE turn_metrics`,
		`truncate table sched.task_iterations restart identity`,
		`TRUNCATE sched.tasks, sched.task_iterations`,
		`TRUNCATE TABLE ONLY sched.tasks , ONLY sched.logs *, sched.task_iterations CASCADE`,
		"TrUnCaTe\nsched.run_logs,\n  turn_metrics",
		`TRUNCATE "chat"."turn_metrics", chat.messages`,
	} {
		if !spendRecordDelete.MatchString(stmt) {
			t.Errorf("not caught: %q", stmt)
		}
	}
	for _, stmt := range []string{
		`DELETE FROM task_iterations_archive`,
		`DELETE FROM my_task_iterations`,
		`TRUNCATE sched.task_iterations_old`,
		`TRUNCATE sched.tasks, sched.logs`,
		`DELETE FROM tasks WHERE id IN (SELECT task_id FROM task_iterations)`,
		`UPDATE task_iterations SET created_by = NULL WHERE created_by = $1`,
		`UPDATE chat.turn_metrics SET user_email = '' WHERE user_email = $1`,
		`SELECT * FROM chat.turn_metrics`,
		`-- the retention sweep deletes from tasks; task_iterations stay`,
	} {
		if spendRecordDelete.MatchString(stmt) {
			t.Errorf("caught, but deletes no spend rows: %q", stmt)
		}
	}
}

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testdb.Empty(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Up(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// spendRecordCascades lists the foreign keys on table that remove or blank
// its rows when the row they reference is deleted (ON DELETE CASCADE, SET
// NULL or SET DEFAULT), as "name (confdeltype x)".
func spendRecordCascades(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT conname, confdeltype::text FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'f' AND confdeltype IN ('c', 'n', 'd')
		ORDER BY conname`, table)
	if err != nil {
		t.Fatalf("%s: read foreign keys: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name, action string
		if err := rows.Scan(&name, &action); err != nil {
			t.Fatal(err)
		}
		out = append(out, name+" (confdeltype "+action+")")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSpendRecordsOutliveTheirParents: no foreign key on a spend record
// removes or blanks its rows when the row it points at is deleted, and no
// function in the database — trigger functions included — deletes from one
// unless spendRecordDeleteAllowedFunctions names it. A reference from a spend
// record to a task, a conversation or an account must leave the record in
// place. (TestTaskIterationAttributionMigration shows the foreign-key query
// finding the cascading key the down migration restores.)
func TestSpendRecordsOutliveTheirParents(t *testing.T) {
	t.Parallel()
	db := openMigrated(t)

	for _, table := range spendRecords {
		for _, fk := range spendRecordCascades(t, db, table) {
			t.Errorf("%s: foreign key %s removes or blanks spend rows when its parent is deleted", table, fk)
		}
	}

	seen := map[string]bool{}
	for _, name := range spendRecordDeletingFunctions(t, db) {
		if _, ok := spendRecordDeleteAllowedFunctions[name]; ok {
			seen[name] = true
			continue
		}
		t.Errorf("function %s deletes from a spend record", name)
	}
	for name := range spendRecordDeleteAllowedFunctions {
		if !seen[name] {
			t.Errorf("stale spendRecordDeleteAllowedFunctions entry: %s deletes from no spend record", name)
		}
	}
}

// spendRecordDeletingFunctions lists the functions of fleet's schemas
// ("<schema>.<name>") whose definition deletes from a spend record. It reads
// pg_get_functiondef rather than prosrc: a SQL-standard BEGIN ATOMIC body is
// stored parsed, with an empty prosrc. Aggregates have no definition to read.
func spendRecordDeletingFunctions(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT n.nspname || '.' || p.proname, pg_catalog.pg_get_functiondef(p.oid)
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname IN ('chat', 'core', 'sched') AND p.prokind IN ('f', 'p')
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("read functions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name, src string
		if err := rows.Scan(&name, &src); err != nil {
			t.Fatal(err)
		}
		if spendRecordDelete.MatchString(src) {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// spendScanFile reports whether the source scan reads a file: Go outside
// tests, SQL (the new-layout up migrations, and any other .sql outside the
// frozen legacy tree), shell and Python scripts, and deploy manifests.
func spendScanFile(name string) bool {
	switch {
	case strings.HasSuffix(name, "_test.go"), strings.HasSuffix(name, ".down.sql"):
		return false
	}
	for _, ext := range []string{".go", ".sql", ".sh", ".py", ".yaml", ".yml", ".tpl"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// TestNoCodeDeletesSpendRecords: nothing in the repo's code, migrations,
// scripts or deploy manifests deletes from a spend record, unless
// spendRecordDeleteAllowedFiles names the file with its reason. Pruning or
// deleting what a spend row describes is fine; the spend row itself stays.
// Down migrations (rollbacks) and the frozen legacy migrations are not read.
func TestNoCodeDeletesSpendRecords(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var bad, walk []string // spend-row deletes; problems with the walk itself
	seen := map[string]bool{}
	scanned := map[string]bool{}
	for _, dir := range []string{"internal", "cmd", "scripts", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				switch {
				case d.Name() == "testdata", d.Name() == "node_modules", rel == "internal/migrations/sql/legacy":
					return filepath.SkipDir
				}
				return nil
			}
			if !spendScanFile(d.Name()) {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned[rel] = true
			loc := spendRecordDelete.FindIndex(body)
			if loc == nil {
				return nil
			}
			if _, ok := spendRecordDeleteAllowedFiles[rel]; ok {
				seen[rel] = true
				return nil
			}
			line := 1 + strings.Count(string(body[:loc[0]]), "\n")
			bad = append(bad, rel+":"+itoa(int64(line)))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for rel := range spendRecordDeleteAllowedFiles {
		if !seen[rel] {
			walk = append(walk, "stale spendRecordDeleteAllowedFiles entry: "+rel)
		}
	}
	// Positive controls: the walk reaches the files that write spend rows and
	// the migration that keeps them, and leaves the frozen legacy tree alone.
	for _, rel := range []string{
		"internal/sched/db/task_iterations.go",
		"internal/sched/db/cleanup.go",
		"scripts/check-migrations.sh",
	} {
		if !scanned[rel] {
			walk = append(walk, "not scanned (the walk missed it): "+rel)
		}
	}
	sawAttributionMigration := false
	for rel := range scanned {
		if strings.HasPrefix(rel, "internal/migrations/sql/legacy/") {
			walk = append(walk, "scanned a frozen legacy migration: "+rel)
		}
		if strings.HasPrefix(rel, "internal/migrations/sql/") && strings.HasSuffix(rel, "_task_iteration_attribution.up.sql") {
			sawAttributionMigration = true
		}
	}
	if !sawAttributionMigration {
		walk = append(walk, "not scanned (the walk missed it): the task_iteration_attribution up migration")
	}
	sort.Strings(walk)
	for _, w := range walk {
		t.Error(w)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s deletes from a spend record (%s)", b, strings.Join(spendRecords, ", "))
	}
}

// attributionMigration returns the version of the migration that gave
// task_iterations its own attribution, and the version before it.
func attributionMigration(t *testing.T) (version, previous int) {
	t.Helper()
	set, err := migrations.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	all := set.All()
	for i, m := range all {
		if strings.HasSuffix(m.Name, "_task_iteration_attribution") {
			return m.Version, all[i-1].Version
		}
	}
	t.Fatal("no task_iteration_attribution migration in the embedded set")
	return 0, 0
}

// downSQL is the down migration of version.
func downSQL(t *testing.T, version int) string {
	t.Helper()
	set, err := migrations.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := set.Get(version)
	if !ok {
		t.Fatalf("no migration %d", version)
	}
	return m.Down
}

// The tasks seeded before 0210 by TestTaskIterationAttributionMigration.
const (
	attrOwner   = "0b5e4a5e-0000-4c1e-9d1e-000000000001"
	attrKeyTask = "0b5e4a5e-0000-4c1e-9d1e-0000000000a1"
	attrOwnTask = "0b5e4a5e-0000-4c1e-9d1e-0000000000a2"
	attrBareTsk = "0b5e4a5e-0000-4c1e-9d1e-0000000000a3"
)

// iterAttribution is one iteration's attribution columns.
type iterAttribution struct{ createdBy, keyID, model sql.NullString }

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// iterationAttributions reads the attribution of taskID's iterations.
func iterationAttributions(t *testing.T, db *sql.DB, taskID string) []iterAttribution {
	t.Helper()
	rows, err := db.Query(`SELECT created_by::text, created_by_key_id, model FROM sched.task_iterations
		WHERE task_id = $1 ORDER BY iteration_number`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []iterAttribution
	for rows.Next() {
		var a iterAttribution
		if err := rows.Scan(&a.createdBy, &a.keyID, &a.model); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// iterationForeignKeys counts task_iterations' foreign keys, and reports
// whether all of them are validated.
func iterationForeignKeys(t *testing.T, db *sql.DB) (n int, validated bool) {
	t.Helper()
	if err := db.QueryRow(`SELECT count(*), COALESCE(bool_and(convalidated), false) FROM pg_catalog.pg_constraint
		WHERE conrelid = 'sched.task_iterations'::regclass AND contype = 'f'`).Scan(&n, &validated); err != nil {
		t.Fatal(err)
	}
	return n, validated
}

func queryCount(t *testing.T, db *sql.DB, q string) (n int) {
	t.Helper()
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func mustExecSQL(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

const countIterationsSQL = `SELECT count(*) FROM sched.task_iterations`

// TestTaskIterationAttributionMigration upgrades a database whose iterations
// predate the attribution columns: each row gets its task's creator, key and
// model, the foreign key is gone so deleting a task keeps its iterations, and
// the down migration restores the cascading key without deleting the rows of
// tasks deleted in between (checkAttributionRollback).
func TestTaskIterationAttributionMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	version, previous := attributionMigration(t)

	db, err := sql.Open("pgx", testdb.Empty(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.UpTo(ctx, db, previous); err != nil {
		t.Fatalf("migrate to %d: %v", previous, err)
	}

	mustExecSQL(t, db, `INSERT INTO sched.tasks (id, prompt, status, priority, created_at, created_by, created_by_key_id, model)
		VALUES ('`+attrKeyTask+`', 'p', 'success', 50, now(), NULL, 'key-1', 'model-a')`)
	mustExecSQL(t, db, `INSERT INTO sched.tasks (id, prompt, status, priority, created_at, created_by, model)
		VALUES ('`+attrOwnTask+`', 'p', 'success', 50, now(), '`+attrOwner+`', 'model-b')`)
	mustExecSQL(t, db, `INSERT INTO sched.tasks (id, prompt, status, priority, created_at)
		VALUES ('`+attrBareTsk+`', 'p', 'success', 50, now())`)
	mustExecSQL(t, db, `INSERT INTO sched.task_iterations (task_id, iteration_number, started_at, cost_usd, status) VALUES
		('`+attrKeyTask+`', 1, now(), 1, 'passed'),
		('`+attrOwnTask+`', 1, now(), 2, 'passed'),
		('`+attrOwnTask+`', 2, now(), 3, 'failed'),
		('`+attrBareTsk+`', 1, now(), 4, 'passed')`)

	rowVersion := func(taskID string) (xmin string) {
		t.Helper()
		if err := db.QueryRow(`SELECT xmin::text FROM sched.task_iterations WHERE task_id = $1`, taskID).Scan(&xmin); err != nil {
			t.Fatal(err)
		}
		return xmin
	}
	bareBefore, keyBefore := rowVersion(attrBareTsk), rowVersion(attrKeyTask)

	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("migrate through %d: %v", version, err)
	}
	// The backfill rewrites only rows that have attribution to copy.
	if rowVersion(attrBareTsk) != bareBefore {
		t.Error("the backfill rewrote the iteration of a task with no attribution")
	}
	if rowVersion(attrKeyTask) == keyBefore {
		t.Error("the backfill did not write the iteration of a task with attribution")
	}
	for taskID, want := range map[string]iterAttribution{
		attrKeyTask: {keyID: nullStr("key-1"), model: nullStr("model-a")},
		attrOwnTask: {createdBy: nullStr(attrOwner), model: nullStr("model-b")},
		attrBareTsk: {},
	} {
		got := iterationAttributions(t, db, taskID)
		if len(got) == 0 {
			t.Fatalf("task %s: no iterations after the migration", taskID)
		}
		for _, a := range got {
			if a != want {
				t.Errorf("task %s: iteration attribution = %+v, want %+v", taskID, a, want)
			}
		}
	}
	if n, _ := iterationForeignKeys(t, db); n != 0 {
		t.Fatalf("task_iterations has %d foreign keys after the migration, want none", n)
	}
	mustExecSQL(t, db, `DELETE FROM sched.tasks WHERE id = $1`, attrOwnTask)
	if n := queryCount(t, db, countIterationsSQL); n != 4 {
		t.Fatalf("iterations after deleting a task = %d, want all 4 kept", n)
	}

	checkAttributionRollback(t, db, version, previous)
}

// checkAttributionRollback takes the database TestTaskIterationAttributionMigration
// left (0210 applied, attrOwnTask deleted) down and up again.
func checkAttributionRollback(t *testing.T, db *sql.DB, version, previous int) {
	t.Helper()
	ctx := context.Background()

	// Down: the cascading key is back (not validated: the deleted task's two
	// iterations stay), and deleting a task deletes its iterations again.
	if _, err := migrations.Down(ctx, db, previous, false); err != nil {
		t.Fatalf("down to %d: %v", previous, err)
	}
	if n, validated := iterationForeignKeys(t, db); n != 1 || validated {
		t.Fatalf("after down: %d foreign keys (validated %v), want the one NOT VALID key", n, validated)
	}
	// The guard's own foreign-key query finds the restored key: it can see a
	// cascading reference when there is one.
	if got, want := spendRecordCascades(t, db, "sched.task_iterations"), []string{"task_iterations_task_id_fkey (confdeltype c)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after down, the guard's foreign-key query = %v, want %v", got, want)
	}
	// The down migration is safe to run again.
	mustExecSQL(t, db, downSQL(t, version))
	if n, _ := iterationForeignKeys(t, db); n != 1 {
		t.Fatalf("after running the down twice: %d foreign keys, want 1", n)
	}
	if n := queryCount(t, db, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sched' AND table_name = 'task_iterations'
		  AND column_name IN ('created_by', 'created_by_key_id', 'model')`); n != 0 {
		t.Fatalf("after down: %d attribution columns, want none", n)
	}
	if n := queryCount(t, db, countIterationsSQL); n != 4 {
		t.Fatalf("after down: %d iterations, want all 4 kept", n)
	}
	mustExecSQL(t, db, `DELETE FROM sched.tasks WHERE id = $1`, attrBareTsk)
	if n := queryCount(t, db, countIterationsSQL); n != 3 {
		t.Fatalf("after down, deleting a task should delete its iteration: %d left, want 3", n)
	}

	// Up again, with the key under another name: the up drops whatever key
	// references tasks, and the live task's iteration is attributed again.
	mustExecSQL(t, db, `ALTER TABLE sched.task_iterations
		RENAME CONSTRAINT task_iterations_task_id_fkey TO task_iterations_task_fk_renamed`)
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n, _ := iterationForeignKeys(t, db); n != 0 {
		t.Fatalf("after up again: %d foreign keys, want the renamed one dropped too", n)
	}
	if got := iterationAttributions(t, db, attrKeyTask); len(got) != 1 || got[0] != (iterAttribution{keyID: nullStr("key-1"), model: nullStr("model-a")}) {
		t.Fatalf("after up again: %+v", got)
	}
	if n := queryCount(t, db, `SELECT count(*) FROM pg_catalog.pg_class WHERE oid = to_regclass('sched.idx_task_iterations_started_at')`); n != 1 {
		t.Fatalf("started_at index after up again: %d, want 1", n)
	}

	// The guard's function query finds a function that deletes spend rows,
	// written with a string body or as a SQL-standard BEGIN ATOMIC body
	// (stored parsed, with an empty prosrc).
	mustExecSQL(t, db, `CREATE FUNCTION sched.prune_spend_probe() RETURNS void LANGUAGE sql
		AS $$ DELETE FROM sched.task_iterations WHERE started_at < now() - interval '1 year' $$`)
	mustExecSQL(t, db, `CREATE FUNCTION sched.prune_spend_atomic_probe() RETURNS void LANGUAGE sql
		BEGIN ATOMIC
			DELETE FROM chat.turn_metrics WHERE completed_at < 0;
		END`)
	if n := queryCount(t, db, `SELECT count(*) FROM pg_catalog.pg_proc
		WHERE proname = 'prune_spend_atomic_probe' AND prosrc = ''`); n != 1 {
		t.Fatalf("the BEGIN ATOMIC probe should have an empty prosrc: %d", n)
	}
	if got, want := spendRecordDeletingFunctions(t, db), []string{"sched.prune_spend_atomic_probe", "sched.prune_spend_probe"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the guard's function query = %v, want %v", got, want)
	}
}
