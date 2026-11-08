package migrations

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/dbschema"
)

// TestAdoption is the table of the states a database can be in when an M6
// build first starts on it.
func TestAdoption(t *testing.T) {
	t.Parallel()
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	all := versionsOf(set)
	// legacyThrough is what adoption records for chat 1..c and sched 1..s.
	legacyThrough := func(c, s int) []int {
		var out []int
		for _, v := range all {
			if (v < SchedOffset && v <= c) || (v > SchedOffset && v-SchedOffset <= s) {
				out = append(out, v)
			}
		}
		return out
	}
	// synthetic writes legacy ledgers only (no schema): refusals happen
	// before any migration would run.
	synthetic := func(chat []int, sched int, dirty bool) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			legacyBuild(t, db, 0, 0)
			for _, v := range chat {
				mustExec(t, db, `INSERT INTO chat.schema_migrations VALUES ($1, 'x', 0)`, v)
			}
			if sched >= 0 {
				mustExec(t, db, `INSERT INTO sched.schema_migrations VALUES ($1, $2)`, sched, dirty)
			}
		}
	}
	cases := []struct {
		name    string
		setup   func(*testing.T, *sql.DB)
		refused string // a substring of the refusal; "" adopts
		adopted []int  // what adoption itself records
	}{
		{name: "fresh database", setup: func(*testing.T, *sql.DB) {}, adopted: nil},
		{name: "at the latest legacy release (chat 99, sched 77)", setup: func(t *testing.T, db *sql.DB) { legacyBuild(t, db, 99, 77) }, adopted: legacyThrough(99, 77)},
		{name: "behind it (chat 95, sched 70)", setup: func(t *testing.T, db *sql.DB) { legacyBuild(t, db, 95, 70) }, adopted: legacyThrough(95, 70)},
		{name: "sched dirty", setup: synthetic([]int{1, 2, 3}, 12, true), refused: "DIRTY at version 12"},
		{name: "unknown chat version", setup: synthetic([]int{1, 2, ChatSentinel}, 3, false), refused: "chat.schema_migrations holds version(s) 100"},
		{name: "sched beyond 77", setup: synthetic([]int{1}, SchedSentinel, false), refused: "outside the legacy range 0–77"},
		{name: "sched ledger with two rows", setup: func(t *testing.T, db *sql.DB) {
			synthetic([]int{1}, 3, false)(t, db)
			mustExec(t, db, `INSERT INTO sched.schema_migrations VALUES (4, false)`)
		}, refused: "holds 2 rows"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := emptyDB(t)
			c.setup(t, db)
			chatBefore, schedBefore := legacyLedgers(t, db)
			r := testEmbeddedRunner(t)

			// adopt alone, under the locks, as up runs it.
			var adopted bool
			err := r.locked(ctx, db, true, func(conn *sql.Conn) error {
				if err := dbschema.Ensure(ctx, conn); err != nil {
					return err
				}
				var err error
				adopted, err = r.adopt(ctx, conn)
				return err
			})
			if c.refused != "" {
				var ae AdoptionError
				if !errors.As(err, &ae) || !strings.Contains(err.Error(), c.refused) {
					t.Fatalf("adopt = %v, want a refusal containing %q", err, c.refused)
				}
				if hasLedger(t, db) {
					t.Fatal("a refused adoption created the ledger")
				}
				if chat, sched := legacyLedgers(t, db); !reflect.DeepEqual(chat, chatBefore) || !reflect.DeepEqual(sched, schedBefore) {
					t.Fatal("a refused adoption changed a legacy ledger")
				}
				if err := r.up(ctx, db); !errors.As(err, &ae) {
					t.Fatalf("up = %v, want the same refusal", err)
				}
				return
			}
			if err != nil || !adopted {
				t.Fatalf("adopt = %v, %v", adopted, err)
			}
			if got := ledgerVersions(t, db); !reflect.DeepEqual(got, c.adopted) {
				t.Fatalf("adopted %v\nwant %v", got, c.adopted)
			}
			if chat, sched := legacyLedgers(t, db); !reflect.DeepEqual(chat, chatBefore) || !reflect.DeepEqual(sched, schedBefore) {
				t.Fatal("adoption changed a legacy ledger")
			}
			// chat keeps when it was applied; sched (which never recorded it)
			// gets the adoption time.
			if len(c.adopted) > 0 {
				var at time.Time
				if err := db.QueryRow(`SELECT applied_at FROM core.schema_versions WHERE version = 7`).Scan(&at); err != nil || at.Unix() != 1700000007 {
					t.Fatalf("chat 7 applied_at = %v (%v), want the legacy ledger's", at, err)
				}
			}
			// Running it again is a no-op.
			err = r.locked(ctx, db, true, func(conn *sql.Conn) error {
				var err error
				adopted, err = r.adopt(ctx, conn)
				return err
			})
			if err != nil || adopted {
				t.Fatalf("second adopt = %v, %v; want a no-op", adopted, err)
			}
			// Up then applies the backlog (all of it for a fresh database) in
			// the merged order, and mirrors legacy versions into the legacy
			// ledgers that exist, so the release before stays a rollback
			// target until a migration >= 200 runs.
			if err := r.up(ctx, db); err != nil {
				t.Fatalf("up: %v", err)
			}
			if got := ledgerVersions(t, db); !reflect.DeepEqual(got, all) {
				t.Fatalf("after up the ledger = %v", got)
			}
			// A fresh database grows complete legacy ledgers too: below the
			// boundary a pre-M6 release can run on it (a rollback).
			chat, sched := legacyLedgers(t, db)
			if len(chat) != 99 || !reflect.DeepEqual(sched, []string{"(77,f)"}) {
				t.Fatalf("legacy ledgers after up: chat %d rows, sched %v", len(chat), sched)
			}
		})
	}
}

// TestAdoptionFailureRollsBack: a failure after the ledger was created and
// filled, before the commit, leaves no trace.
func TestAdoptionFailureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	legacyBuild(t, db, 99, 77)
	r := testEmbeddedRunner(t)
	r.beforeAdoptCommit = func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM core.schema_versions`).Scan(&n); err != nil {
			return err
		}
		if n != 99+77 {
			t.Errorf("adoption wrote %d rows before the failure", n)
		}
		return errors.New("injected failure")
	}
	if err := r.up(ctx, db); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("up = %v", err)
	}
	if hasLedger(t, db) {
		t.Fatal("the failed adoption left core.schema_versions behind")
	}
	r.beforeAdoptCommit = nil
	if err := r.up(ctx, db); err != nil {
		t.Fatalf("up after the failure: %v", err)
	}
}

// TestStatus: the read-only report before adoption (what adoption will
// record), after it, and the two derived pre-M6 views.
func TestStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := testEmbeddedRunner(t)

	fresh := emptyDB(t)
	rep, err := r.status(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Adopted || len(rep.Applied) != 0 || len(rep.Pending) != 99+77 || rep.CurrentVersion != nil || rep.Layout != "unified" {
		t.Fatalf("fresh: %+v", rep)
	}
	if hasLedger(t, fresh) {
		t.Fatal("status created the ledger")
	}

	db := emptyDB(t)
	legacyBuild(t, db, 95, 70)
	rep, err = r.status(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Adopted || len(rep.Applied) != 95+70 || len(rep.Pending) != 4+7 || *rep.CurrentVersion != 170 || !strings.Contains(rep.Legacy, "sched: 70") {
		t.Fatalf("legacy 95/70: adopted %v applied %d pending %d current %v legacy %q", rep.Adopted, len(rep.Applied), len(rep.Pending), rep.CurrentVersion, rep.Legacy)
	}
	if sv := rep.Sched(); *sv.CurrentVersion != 70 || len(sv.Applied) != 70 || len(sv.Pending) != 7 || sv.Pending[0].Name != "071_recurrence_unspawned_includes_dead_lettered" {
		t.Fatalf("sched view: %+v", sv)
	}
	if cv := rep.Chat(); len(cv.Applied) != 95 || len(cv.Pending) != 4 || cv.Applied[0].Version != 1 || cv.Applied[0].AppliedAt == nil || cv.DB != "chat" {
		t.Fatalf("chat view: %+v", cv)
	}

	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}
	rep, err = r.status(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Adopted || len(rep.Pending) != 0 || *rep.CurrentVersion != LegacyLatest || rep.Dirty || rep.Legacy != "" {
		t.Fatalf("migrated: %+v", rep)
	}
	if sv := rep.Sched(); *sv.CurrentVersion != 77 || sv.Runner != "golang-migrate" {
		t.Fatalf("sched view: %+v", sv)
	}
}

// TestLegacyLedgersStayAuthoritative (Codex 4506315485): below the boundary a
// pre-M6 release may run on an adopted database and move the legacy ledgers
// — apply what the one ledger lacks, or roll something back. The fast path
// notices, and Up reconciles the one ledger to them instead of re-running
// applied files (099's ALTER TABLE users SET SCHEMA core would fail) or
// skipping rolled-back ones.
func TestLegacyLedgersStayAuthoritative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	legacyBuild(t, db, 99, 77)
	r := testEmbeddedRunner(t)
	if err := r.up(ctx, db); err != nil {
		t.Fatal(err)
	}

	// A pre-M6 binary applied 099 and sched 077 that the one ledger does not
	// record (as if adopted before they ran, then rolled back and forward).
	mustExec(t, db, `DELETE FROM core.schema_versions WHERE version IN (99, 177)`)
	if r.current(ctx, db) {
		t.Fatal("the fast path missed legacy progress")
	}
	if err := r.up(ctx, db); err != nil {
		t.Fatalf("up after legacy progress (must reconcile, not re-run 099/177): %v", err)
	}
	if got := ledgerVersions(t, db); len(got) != 176 {
		t.Fatalf("reconciled ledger has %d rows", len(got))
	}

	// A pre-M6 binary's operator rolled sched 077 back (its .down.sql, the
	// legacy row to 76): Up applies 177 again.
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	m177, _ := set.Get(177)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, `SET search_path TO sched`)
	mustExec(t, conn, m177.Down)
	mustExec(t, conn, `RESET search_path`)
	_ = conn.Close()
	mustExec(t, db, `UPDATE sched.schema_migrations SET version = 76`)
	var back bool
	if err := db.QueryRow(`SELECT to_regclass('chat.chat_schedules') IS NOT NULL`).Scan(&back); err != nil || !back {
		t.Fatalf("077's down did not put chat.chat_schedules back (%v)", err)
	}
	if r.current(ctx, db) {
		t.Fatal("the fast path missed a legacy rollback")
	}
	if err := r.up(ctx, db); err != nil {
		t.Fatalf("up after a legacy rollback: %v", err)
	}
	if err := db.QueryRow(`SELECT to_regclass('chat.chat_schedules') IS NOT NULL`).Scan(&back); err != nil || back {
		t.Fatalf("177 was not applied again (chat.chat_schedules still there: %v, %v)", back, err)
	}
	_, sched := legacyLedgers(t, db)
	if !reflect.DeepEqual(sched, []string{"(77,f)"}) || !r.current(ctx, db) {
		t.Fatalf("after re-applying 177: sched ledger %v, current %v", sched, r.current(ctx, db))
	}

	// A legacy state this build cannot continue from (dirty) is refused by
	// Up and reported as a problem by status.
	mustExec(t, db, `UPDATE sched.schema_migrations SET dirty = true`)
	var ae AdoptionError
	if err := r.up(ctx, db); !errors.As(err, &ae) {
		t.Fatalf("up over a dirty legacy row = %v", err)
	}
	if rep, err := r.status(ctx, db); err != nil || rep.Problem == "" || !rep.Refused() {
		t.Fatalf("status over a dirty legacy row: problem %q, refused %v, %v", rep.Problem, rep.Refused(), err)
	}
}

// TestStatusKeepsTheAdoptionProblem (Codex 4506315482): before adoption, a
// legacy state adoption refuses makes the report Refused.
func TestStatusKeepsTheAdoptionProblem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := emptyDB(t)
	legacyBuild(t, db, 0, 0)
	mustExec(t, db, `INSERT INTO sched.schema_migrations VALUES (3, false), (4, false)`)
	r := testEmbeddedRunner(t)
	rep, err := r.status(ctx, db)
	if err != nil || !strings.Contains(rep.Problem, "holds 2 rows") || !rep.Refused() {
		t.Fatalf("status = problem %q refused %v (%v)", rep.Problem, rep.Refused(), err)
	}
	var ae AdoptionError
	if err := r.up(ctx, db); !errors.As(err, &ae) {
		t.Fatalf("up = %v, want the same refusal", err)
	}
}
