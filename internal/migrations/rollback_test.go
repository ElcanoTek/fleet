package migrations

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rollbackBlock is the SQL between "-- <name>-begin" and "-- <name>-end" in
// docs/MIGRATIONS.md.
func rollbackBlock(t *testing.T, name string) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "MIGRATIONS.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "-- "+name+"-begin\n")
	if !ok {
		t.Fatalf("docs/MIGRATIONS.md has no %s block", name)
	}
	block, _, ok := strings.Cut(rest, "-- "+name+"-end")
	if !ok {
		t.Fatalf("docs/MIGRATIONS.md: the %s block is not closed", name)
	}
	return block
}

// TestM6RollbackBlock runs docs/MIGRATIONS.md's M6 rollback on a box adopted
// while behind (chat 95, sched 70), whose backlog the runner applied: the
// legacy ledgers it leaves say exactly what the release before M6 would have
// recorded (chat 1–99, sched 77), and a later M6 start adopts again.
func TestM6RollbackBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	legacyBuild(t, db, 95, 70)
	r := testEmbeddedRunner(t)
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, rollbackBlock(t, "m6-rollback"))
	if hasLedger(t, db) {
		t.Fatal("the rollback left core.schema_versions")
	}
	chat, sched := legacyLedgers(t, db)
	if len(chat) != 99 || !reflect.DeepEqual(sched, []string{"(77,f)"}) {
		t.Fatalf("legacy ledgers after the rollback: chat %d rows, sched %v; want what the previous release records", len(chat), sched)
	}
	var top int
	if err := db.QueryRow(`SELECT max(version) FROM chat.schema_migrations`).Scan(&top); err != nil || top != 99 {
		t.Fatalf("chat ledger max = %d (%v)", top, err)
	}
	if err := r.up(ctx, db); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if got := ledgerVersions(t, db); !reflect.DeepEqual(got, versionsOf(r.set)) {
		t.Fatalf("re-adopted ledger = %v", got)
	}
}
