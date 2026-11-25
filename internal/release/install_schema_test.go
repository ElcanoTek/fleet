// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// schemaBase is the oldest version the fake ledger holds.
const schemaBase = 210

// schemaWorld is one box: a service running whichever binary is live, and the
// database it migrates. Each binary ships migrations up to ships[bytes]; on
// start it refuses a database at a newer version (migrations.
// UnknownVersionError: the process exits, systemd restarts it forever) and
// otherwise migrates the database up to its own version, as every store open
// does. It is ready only when it did not refuse and ready[bytes] is set — so
// a new release that migrates and then fails its gate (a slow boot, a later
// boot step failing) is ships["NEW"] > ships["OLD"] with ready["NEW"] false.
//
// It is also the Schema of the binary running the install or rollback, which
// ships down migrations up to downShips (the NEW release when update.sh runs
// it; an operator may run the OLD one).
type schemaWorld struct {
	t         *testing.T
	dir       string
	db        int // the ledger holds schemaBase..db
	ships     map[string]int
	ready     map[string]bool
	downShips int
	running   string // the binary the service runs; "" when stopped
	refused   bool   // the running binary refused the database

	events      []string // start:<bin>, stop, down:<v>(live=<bin>)
	versionErrs []error  // returned by the next Applied calls, in order
	downErr     error
	startErr    map[string]error
	reads       int // Applied calls
}

// newSchemaWorld starts with OLD live and running on the database it migrated.
func newSchemaWorld(t *testing.T) (*schemaWorld, string) {
	t.Helper()
	const live = "OLD"
	dir, bin, _, _ := setup(t, live)
	w := &schemaWorld{
		t: t, dir: dir,
		ships:    map[string]int{"OLD": 214, "NEW": 216},
		ready:    map[string]bool{"OLD": true},
		startErr: map[string]error{},
	}
	w.downShips = w.ships["NEW"]
	w.db = w.ships[live]
	w.running = live
	return w, bin
}

func (w *schemaWorld) live() string {
	b, err := os.ReadFile(filepath.Join(w.dir, "fleet"))
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func (w *schemaWorld) Stop(context.Context, string) error {
	w.events = append(w.events, "stop")
	w.running, w.refused = "", false
	return nil
}

func (w *schemaWorld) Start(context.Context, string) error {
	bin := w.live()
	w.events = append(w.events, "start:"+bin)
	if err := w.startErr[bin]; err != nil {
		return err
	}
	w.running = bin
	w.refused = w.db > w.ships[bin]
	if !w.refused && w.db < w.ships[bin] {
		w.db = w.ships[bin]
	}
	return nil
}

func (w *schemaWorld) Restart(ctx context.Context, svc string) error {
	_ = w.Stop(ctx, svc)
	return w.Start(ctx, svc)
}

func (w *schemaWorld) WaitReady(context.Context, string, time.Duration) error {
	switch {
	case w.running == "":
		return errors.New("connection refused")
	case w.refused:
		return fmt.Errorf("connection refused (%s refuses the database at version %d: it ships up to %d)", w.running, w.db, w.ships[w.running])
	case !w.ready[w.running]:
		return errors.New("HTTP 503")
	}
	return nil
}

func versionsTo(top int) []int {
	var out []int
	for v := schemaBase; v <= top; v++ {
		out = append(out, v)
	}
	return out
}

func (w *schemaWorld) Applied(context.Context) ([]int, error) {
	w.reads++
	if len(w.versionErrs) > 0 {
		err := w.versionErrs[0]
		w.versionErrs = w.versionErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return versionsTo(w.db), nil
}

func (w *schemaWorld) DownTo(_ context.Context, v int) ([]int, error) {
	w.events = append(w.events, fmt.Sprintf("down:%d(live=%s)", v, w.live()))
	if w.running != "" {
		w.t.Errorf("down migrations ran while the service was running %s", w.running)
	}
	if w.downErr != nil {
		return nil, w.downErr
	}
	if w.db > w.downShips {
		return nil, fmt.Errorf("the database has migration version(s) above %d that this build does not ship", w.downShips)
	}
	var undone []int
	for x := w.db; x > v; x-- {
		undone = append(undone, x)
	}
	w.db = v
	return undone, nil
}

func (w *schemaWorld) opts(bin string) InstallOptions {
	o := opts(w.dir, bin, w, w)
	o.Schema = w
	return o
}

// record is the newest version fleet.prev.schema records.
func (w *schemaWorld) record() string {
	b, err := os.ReadFile(filepath.Join(w.dir, schemaRecordName))
	if errors.Is(err, os.ErrNotExist) {
		return "<none>"
	}
	if err != nil {
		w.t.Fatal(err)
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return "<empty>"
	}
	return f[len(f)-1]
}

// failRenames makes the next n renames into the live path fail.
func failRenames(t *testing.T, n int) {
	t.Helper()
	old := renameIntoLive
	left := n
	renameIntoLive = func(from, to string) error {
		if left > 0 {
			left--
			return errors.New("rename: input/output error")
		}
		return old(from, to)
	}
	t.Cleanup(func() { renameIntoLive = old })
}

func fastReads(t *testing.T) {
	t.Helper()
	old := schemaReadInterval
	schemaReadInterval = time.Millisecond
	t.Cleanup(func() { schemaReadInterval = old })
}

// TestInstallRollbackUndoesTheNewMigrations is the failure this guards
// against: a release that ships a migration applies it on start, then fails
// its readiness gate. Swapping fleet.prev back in without touching the schema
// leaves it refusing the database (the first subtest is that behaviour, with
// no Schema). With Schema the rollback stops the service, puts fleet.prev in
// place, undoes the new release's migrations down to what the database held
// before the restart, and only then starts fleet.prev, which comes up.
func TestInstallRollbackUndoesTheNewMigrations(t *testing.T) {
	t.Run("without Schema the previous binary refuses the database", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		o := w.opts(bin)
		o.Schema = nil
		_, err := Install(context.Background(), o)
		if err == nil || errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "refuses the database") {
			t.Fatalf("got %v, want the rollback to fail on the refused schema", err)
		}
		if w.live() != "OLD" || !w.refused || w.db != 216 {
			t.Fatalf("live %s refused %v db %d", w.live(), w.refused, w.db)
		}
	})

	w, bin := newSchemaWorld(t)
	res, err := Install(context.Background(), w.opts(bin))
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if w.live() != "OLD" || w.running != "OLD" || w.refused {
		t.Fatalf("live %s running %q refused %v: the previous binary is not serving", w.live(), w.running, w.refused)
	}
	if w.db != 214 {
		t.Errorf("database at %d, want 214 (what the previous binary ran on)", w.db)
	}
	if !reflect.DeepEqual(res.UndoneMigrations, []int{216, 215}) {
		t.Errorf("UndoneMigrations %v", res.UndoneMigrations)
	}
	// Stopped, fleet.prev live before the down migrations, then started.
	if want := "stop,start:NEW,stop,down:214(live=OLD),stop,start:OLD"; strings.Join(w.events, ",") != want {
		t.Errorf("events %v, want %s", w.events, want)
	}
	if read(t, filepath.Join(w.dir, failedName)) != "NEW" || read(t, filepath.Join(w.dir, prevName)) != "OLD" {
		t.Error("fleet.failed / fleet.prev not kept")
	}
	if w.record() != "214" {
		t.Errorf("%s ends at %s, want 214", schemaRecordName, w.record())
	}
}

// TestInstallRollbackWithTheDrainHandoff: update.sh hands the install the
// drain marker (ClearDrain), so restarts are stop + clear + start; the schema
// undo slots in after the failed gate and before fleet.prev's start.
func TestInstallRollbackWithTheDrainHandoff(t *testing.T) {
	w, bin := newSchemaWorld(t)
	o := w.opts(bin)
	clears := 0
	o.ClearDrain = func() error { clears++; return nil }
	if _, err := Install(context.Background(), o); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("got %v", err)
	}
	if w.running != "OLD" || w.refused || w.db != 214 || clears != 2 {
		t.Fatalf("running %q refused %v db %d clears %d", w.running, w.refused, w.db, clears)
	}
}

// TestInstallRunByTheOldBinary: an operator who runs `fleet release install`
// with the installed (older) binary rather than the new one gets a rollback
// whose binary does not ship the new versions' down migrations. It must not
// assume "nothing newer, nothing to undo": it reads the database, sees the new
// versions, its Down refuses them, and the new binary is kept — never the
// refusal loop.
func TestInstallRunByTheOldBinary(t *testing.T) {
	w, bin := newSchemaWorld(t)
	w.downShips = w.ships["OLD"]
	_, err := Install(context.Background(), w.opts(bin))
	if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "does not ship") {
		t.Fatalf("got %v", err)
	}
	if w.live() != "NEW" || w.running != "NEW" || w.refused || w.db != 216 {
		t.Fatalf("live %s running %q refused %v db %d", w.live(), w.running, w.refused, w.db)
	}
	if _, err := os.Stat(filepath.Join(w.dir, failedName)); !os.IsNotExist(err) {
		t.Error("fleet.failed left behind although that binary is live")
	}
}

// TestInstallRefusesWithoutTheSchemaVersions: with a rollback armed, the
// versions the database holds must be known before the new binary starts, or
// a rollback could not know what to undo. The install refuses and changes
// nothing. --no-rollback (and --no-restart) do not need them.
func TestInstallRefusesWithoutTheSchemaVersions(t *testing.T) {
	w, bin := newSchemaWorld(t)
	w.versionErrs = []error{errors.New("connection refused")}
	_, err := Install(context.Background(), w.opts(bin))
	if !errors.Is(err, ErrNothingChanged) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("got %v", err)
	}
	if w.live() != "OLD" || len(w.events) != 0 {
		t.Fatalf("live %s events %v: the refusal changed something", w.live(), w.events)
	}
	if _, err := os.Stat(filepath.Join(w.dir, prevName)); !os.IsNotExist(err) {
		t.Error("fleet.prev was written")
	}

	for _, tc := range []struct {
		name string
		set  func(*InstallOptions)
	}{
		{"no-rollback", func(o *InstallOptions) { o.NoRollback = true }},
		{"no-restart", func(o *InstallOptions) { o.NoRestart = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, bin := newSchemaWorld(t)
			w.ready["NEW"] = true
			w.versionErrs = []error{errors.New("connection refused")}
			if err := os.WriteFile(filepath.Join(w.dir, schemaRecordName), []byte("199\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			o := w.opts(bin)
			tc.set(&o)
			if _, err := Install(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if w.live() != "NEW" {
				t.Fatal("not installed")
			}
			if w.record() != "<none>" {
				t.Errorf("the record of the replaced fleet.prev was kept (%s): it describes another binary", w.record())
			}
		})
	}
}

// TestInstallKeepsTheNewBinaryWhenTheMigrationsCannotBeUndone: fleet.prev
// would refuse the database, so the box stays on the new binary — the one
// that runs on it — restarted, and the error says so.
func TestInstallKeepsTheNewBinaryWhenTheMigrationsCannotBeUndone(t *testing.T) {
	t.Run("down fails", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		w.downErr = errors.New("down 216: canceling statement due to lock timeout")
		_, err := Install(context.Background(), w.opts(bin))
		if !errors.Is(err, ErrKeptNew) || errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "lock timeout") || !strings.Contains(err.Error(), "not ready either") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.running != "NEW" || w.refused {
			t.Fatalf("live %s running %q refused %v: the new binary was not put back", w.live(), w.running, w.refused)
		}
		if _, err := os.Stat(filepath.Join(w.dir, failedName)); !os.IsNotExist(err) {
			t.Error("fleet.failed left behind although that binary is live")
		}
		if read(t, filepath.Join(w.dir, prevName)) != "OLD" || w.record() != "214" {
			t.Error("fleet.prev or its record changed")
		}
	})
	t.Run("restarted new binary is ready", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		w.downErr = errors.New("boom")
		o := w.opts(bin)
		probes := 0
		o.Prober = proberFunc(func(ctx context.Context, url string, d time.Duration) error {
			probes++
			w.ready["NEW"] = probes > 1 // a slow first boot: ready on the restart
			return w.WaitReady(ctx, url, d)
		})
		res, err := Install(context.Background(), o)
		if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "ready now") || !res.Restarted {
			t.Fatalf("got %v, %+v", err, res)
		}
	})
	t.Run("database unreadable after the gate", func(t *testing.T) {
		fastReads(t)
		w, bin := newSchemaWorld(t)
		down := errors.New("connection refused")
		w.versionErrs = []error{nil, down, down, down, down, down}
		_, err := Install(context.Background(), w.opts(bin))
		if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.reads != 1+schemaReadAttempts {
			t.Fatalf("live %s, %d reads", w.live(), w.reads)
		}
	})
	t.Run("nothing recorded before", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		o := w.opts(bin)
		o.Schema = &emptyFirst{Schema: w}
		_, err := Install(context.Background(), o)
		if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "recorded no migrations before") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" {
			t.Fatal("swapped to a binary that refuses the database")
		}
	})
}

// emptyFirst reports an empty ledger on its first read.
type emptyFirst struct {
	Schema
	done bool
}

func (e *emptyFirst) Applied(ctx context.Context) ([]int, error) {
	vs, err := e.Schema.Applied(ctx)
	if !e.done {
		e.done = true
		return nil, err
	}
	return vs, err
}

type proberFunc func(context.Context, string, time.Duration) error

func (f proberFunc) WaitReady(ctx context.Context, url string, d time.Duration) error {
	return f(ctx, url, d)
}

// TestInstallRollbackRetriesTheVersionRead: a database that stops answering
// for a moment is one of the reasons a gate fails; the rollback retries.
func TestInstallRollbackRetriesTheVersionRead(t *testing.T) {
	fastReads(t)
	w, bin := newSchemaWorld(t)
	blip := errors.New("connection reset")
	w.versionErrs = []error{nil, blip, blip}
	if _, err := Install(context.Background(), w.opts(bin)); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("got %v", err)
	}
	if w.db != 214 || w.running != "OLD" || w.refused {
		t.Fatalf("db %d running %q refused %v", w.db, w.running, w.refused)
	}
}

// TestInstallRollbackWithNothingToUndo: a release that applied no migration
// (it ships none newer, or it failed before migrating) is rolled back after
// one read of the database, with no down migration.
func TestInstallRollbackWithNothingToUndo(t *testing.T) {
	for name, set := range map[string]func(*schemaWorld){
		"ships no newer version": func(w *schemaWorld) { w.ships["NEW"] = 214 },
		"failed before migrating": func(w *schemaWorld) {
			w.startErr["NEW"] = errors.New("sandbox preflight: no runtime")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, bin := newSchemaWorld(t)
			set(w)
			if _, err := Install(context.Background(), w.opts(bin)); !errors.Is(err, ErrRolledBack) {
				t.Fatalf("got %v", err)
			}
			for _, e := range w.events {
				if strings.HasPrefix(e, "down:") {
					t.Errorf("events %v: nothing was migrated, nothing should be undone", w.events)
				}
			}
			if w.db != 214 || w.running != "OLD" || w.refused || w.reads != 2 {
				t.Fatalf("db %d running %q refused %v reads %d", w.db, w.running, w.refused, w.reads)
			}
		})
	}
}

// TestInstallRestartsWhenTheRestoreFails: the service was stopped for the
// schema; if fleet.prev cannot be put back, the new binary — still live — is
// started again rather than left stopped.
func TestInstallRestartsWhenTheRestoreFails(t *testing.T) {
	w, bin := newSchemaWorld(t)
	o := w.opts(bin)
	probes := 0
	o.Prober = proberFunc(func(context.Context, string, time.Duration) error {
		probes++
		if probes == 1 {
			failRenames(t, 1) // the next rename is fleet.prev's restore
			return errors.New("HTTP 503")
		}
		return nil
	})
	res, err := Install(context.Background(), o)
	if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "restore") || !strings.Contains(err.Error(), "ready now") || !res.Restarted {
		t.Fatalf("got %v, %+v", err, res)
	}
	if w.live() != "NEW" || w.running != "NEW" {
		t.Fatalf("live %s running %q: left stopped", w.live(), w.running)
	}
	if w.db != 216 {
		t.Errorf("db %d: nothing should have been undone", w.db)
	}
}

// TestInstallWithoutAStopKeepsTheNewBinary: down migrations never run under a
// live process; a restarter that cannot stop the service on its own cannot
// give them that, so the new binary is kept.
func TestInstallWithoutAStopKeepsTheNewBinary(t *testing.T) {
	w, bin := newSchemaWorld(t)
	o := w.opts(bin)
	o.Restarter = restartOnlyWorld{w}
	_, err := Install(context.Background(), o)
	if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "only restarts") {
		t.Fatalf("got %v", err)
	}
	if w.live() != "NEW" {
		t.Fatal("swapped to a binary that refuses the database")
	}
}

type restartOnlyWorld struct{ w *schemaWorld }

func (r restartOnlyWorld) Restart(ctx context.Context, svc string) error {
	return r.w.Restart(ctx, svc)
}

// installedNewOverOld leaves the box after a successful install of NEW over
// OLD: NEW live and migrated to 216, OLD as fleet.prev recorded at 214.
func installedNewOverOld(t *testing.T) *schemaWorld {
	t.Helper()
	w, bin := newSchemaWorld(t)
	w.ready["NEW"] = true
	if _, err := Install(context.Background(), w.opts(bin)); err != nil {
		t.Fatal(err)
	}
	if w.db != 216 || w.record() != "214" {
		t.Fatalf("db %d record %s after the install", w.db, w.record())
	}
	w.events = nil
	return w
}

// TestRollbackPastAMigration: `fleet release rollback` to a binary that ran
// on an older schema undoes the newer migrations — only when asked
// (--migrate-down), since days after an install they hold real data — and
// never with --no-restart.
func TestRollbackPastAMigration(t *testing.T) {
	t.Run("refused without --migrate-down", func(t *testing.T) {
		w := installedNewOverOld(t)
		err := Rollback(context.Background(), w.opts(""))
		if !errors.Is(err, ErrMigrateDownNeeded) || !strings.Contains(err.Error(), "--migrate-down") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.db != 216 || len(w.events) != 0 || w.record() != "214" {
			t.Fatalf("live %s db %d events %v record %s: the refusal changed something", w.live(), w.db, w.events, w.record())
		}
	})
	t.Run("refused with --no-restart", func(t *testing.T) {
		w := installedNewOverOld(t)
		o := w.opts("")
		o.NoRestart, o.MigrateDown = true, true
		err := Rollback(context.Background(), o)
		if !errors.Is(err, ErrMigrateDownNeeded) || !strings.Contains(err.Error(), "fleet migrate down --to 214") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.db != 216 {
			t.Fatal("the refusal changed something")
		}
	})
}

// TestRollbackMigrateDown: with --migrate-down the rollback stops the
// service, swaps, undoes the newer migrations and starts the older binary;
// rolling forward again needs no down.
func TestRollbackMigrateDown(t *testing.T) {
	t.Run("--migrate-down", func(t *testing.T) {
		w := installedNewOverOld(t)
		o := w.opts("")
		o.MigrateDown = true
		if err := Rollback(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if w.live() != "OLD" || w.running != "OLD" || w.refused || w.db != 214 {
			t.Fatalf("live %s running %q refused %v db %d", w.live(), w.running, w.refused, w.db)
		}
		if want := "stop,down:214(live=OLD),stop,start:OLD"; strings.Join(w.events, ",") != want {
			t.Errorf("events %v, want %s", w.events, want)
		}
		if read(t, filepath.Join(w.dir, prevName)) != "NEW" || w.record() != "216" {
			t.Errorf("fleet.prev %s record %s, want NEW at 216", read(t, filepath.Join(w.dir, prevName)), w.record())
		}
		// Rolling forward again needs no down: NEW migrates up on start.
		w.events = nil
		if err := Rollback(context.Background(), w.opts("")); err != nil {
			t.Fatal(err)
		}
		if w.live() != "NEW" || w.db != 216 || w.running != "NEW" || w.record() != "214" {
			t.Fatalf("live %s db %d running %q record %s", w.live(), w.db, w.running, w.record())
		}
	})
}

// TestRollbackMigrateDownFailures: a failed down migration undoes the swap,
// a failed swap restarts the newer binary; neither leaves fleet stopped.
func TestRollbackMigrateDownFailures(t *testing.T) {
	t.Run("down fails: the swap is undone", func(t *testing.T) {
		w := installedNewOverOld(t)
		w.downErr = errors.New("boom")
		o := w.opts("")
		o.MigrateDown = true
		err := Rollback(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "the swap was undone") || !strings.Contains(err.Error(), "restarted on the live binary") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.running != "NEW" || w.refused || w.record() != "214" || read(t, filepath.Join(w.dir, prevName)) != "OLD" {
			t.Fatalf("live %s running %q refused %v record %s", w.live(), w.running, w.refused, w.record())
		}
	})
	t.Run("swap fails after the stop", func(t *testing.T) {
		w := installedNewOverOld(t)
		failRenames(t, 1)
		o := w.opts("")
		o.MigrateDown = true
		err := Rollback(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "restarted on the live binary") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.running != "NEW" || w.db != 216 {
			t.Fatalf("live %s running %q db %d: left stopped or half-done", w.live(), w.running, w.db)
		}
		if read(t, filepath.Join(w.dir, prevName)) != "OLD" || w.record() != "214" {
			t.Errorf("fleet.prev %s record %s: the previous binary was lost", read(t, filepath.Join(w.dir, prevName)), w.record())
		}
	})
}

// TestRollbackWithoutARecord: with no record of the versions fleet.prev ran
// on, a rollback cannot tell whether fleet.prev runs on the database — if the
// release being left applied a migration, swapping blind starts a binary that
// refuses it. So it refuses, changing nothing, until the operator says so
// (WithoutSchemaCheck), which swaps the binaries only.
func TestRollbackWithoutARecord(t *testing.T) {
	noRecord := func(t *testing.T) *schemaWorld {
		t.Helper()
		w := installedNewOverOld(t)
		if err := os.Remove(filepath.Join(w.dir, schemaRecordName)); err != nil {
			t.Fatal(err)
		}
		return w
	}
	t.Run("refused", func(t *testing.T) {
		w := noRecord(t)
		reads := w.reads
		err := Rollback(context.Background(), w.opts(""))
		if !errors.Is(err, ErrNothingChanged) || !strings.Contains(err.Error(), "--without-schema-check") || !strings.Contains(err.Error(), "fleet migrate down") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.running != "NEW" || len(w.events) != 0 || w.reads != reads {
			t.Fatalf("live %s running %q events %v reads %d: the refusal changed something", w.live(), w.running, w.events, w.reads-reads)
		}
	})
	t.Run("--without-schema-check swaps the binaries only", func(t *testing.T) {
		w := noRecord(t)
		o := w.opts("")
		o.WithoutSchemaCheck = true
		_ = Rollback(context.Background(), o) // OLD then refuses the database: the operator's call
		for _, e := range w.events {
			if strings.HasPrefix(e, "down:") {
				t.Fatalf("events %v: no down without the check", w.events)
			}
		}
		if w.live() != "OLD" || w.record() != "216" {
			t.Fatalf("live %s record %s", w.live(), w.record())
		}
	})
	t.Run("--without-schema-check after a manual down", func(t *testing.T) {
		w := noRecord(t)
		_ = w.Stop(context.Background(), "fleet")
		w.db = 214 // the operator ran fleet migrate down --to 214
		o := w.opts("")
		o.WithoutSchemaCheck = true
		if err := Rollback(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if w.live() != "OLD" || w.running != "OLD" || w.refused {
			t.Fatalf("live %s running %q refused %v", w.live(), w.running, w.refused)
		}
	})
	t.Run("unreadable database with a record", func(t *testing.T) {
		w := installedNewOverOld(t)
		w.versionErrs = []error{errors.New("connection refused")}
		err := Rollback(context.Background(), w.opts(""))
		if !errors.Is(err, ErrNothingChanged) || !strings.Contains(err.Error(), "--without-schema-check") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || len(w.events) != 0 {
			t.Fatal("the refusal changed something")
		}
	})
	t.Run("both flags", func(t *testing.T) {
		w := installedNewOverOld(t)
		o := w.opts("")
		o.WithoutSchemaCheck, o.MigrateDown = true, true
		if err := Rollback(context.Background(), o); !errors.Is(err, ErrNothingChanged) || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" {
			t.Fatal("swapped")
		}
	})
}

// TestFirstInstallReadsNoSchema: with no live binary there is no fleet.prev to
// roll back to or to record a schema for, so the database is not read.
func TestFirstInstallReadsNoSchema(t *testing.T) {
	w, bin := newSchemaWorld(t)
	if err := os.Remove(filepath.Join(w.dir, "fleet")); err != nil {
		t.Fatal(err)
	}
	w.ready["NEW"] = true
	w.versionErrs = []error{errors.New("connection refused")}
	if _, err := Install(context.Background(), w.opts(bin)); err != nil {
		t.Fatal(err)
	}
	if w.reads != 0 || w.record() != "<none>" {
		t.Fatalf("%d reads, record %s", w.reads, w.record())
	}
}

func TestUndoTarget(t *testing.T) {
	for _, c := range []struct {
		name        string
		before, now []int
		to          int
		need        bool
		err         string
	}{
		{"nothing new", []int{1, 2, 3}, []int{1, 2, 3}, 0, false, ""},
		{"a tail", []int{1, 2, 3}, []int{1, 2, 3, 4, 5}, 3, true, ""},
		{"fewer now", []int{1, 2, 3}, []int{1, 2}, 0, false, ""},
		{"empty before", nil, []int{1}, 0, false, "recorded no migrations before"},
		{"not a tail", []int{1, 3}, []int{1, 2, 3}, 0, false, "not all above 3"},
	} {
		to, need, err := undoTarget(c.before, c.now)
		if to != c.to || need != c.need || (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: undoTarget = %d, %v, %v", c.name, to, need, err)
		}
	}
}

// TestInstallNeverRestoresAStaleFailedBinary: fleet.failed is how the new
// binary comes back when the down migrations fail. One left by an earlier
// failed install is removed when an install starts, and when the new binary
// cannot be kept as fleet.failed (the link's copy fallback out of space) the
// rollback does not swap at all: the new binary stays, restarted.
func TestInstallNeverRestoresAStaleFailedBinary(t *testing.T) {
	stale := func(t *testing.T, w *schemaWorld) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(w.dir, failedName), []byte("STALE"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("keeping fleet.failed fails", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		stale(t, w)
		w.downErr = errors.New("boom") // would restore fleet.failed if it got that far
		old := replaceLink
		replaceLink = func(src, dst string) error {
			if filepath.Base(dst) == failedName {
				return errors.New("copy: no space left on device")
			}
			return old(src, dst)
		}
		t.Cleanup(func() { replaceLink = old })
		_, err := Install(context.Background(), w.opts(bin))
		if !errors.Is(err, ErrKeptNew) || !strings.Contains(err.Error(), "no space left") {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" || w.running != "NEW" || w.db != 216 {
			t.Fatalf("live %s running %q db %d", w.live(), w.running, w.db)
		}
		for _, e := range w.events {
			if strings.HasPrefix(e, "down:") {
				t.Fatalf("events %v: nothing should be undone without a way back", w.events)
			}
		}
		if _, err := os.Stat(filepath.Join(w.dir, failedName)); !os.IsNotExist(err) {
			t.Error("a fleet.failed is still there")
		}
	})
	t.Run("down fails: the new binary, not the stale one, comes back", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		stale(t, w)
		w.downErr = errors.New("boom")
		if _, err := Install(context.Background(), w.opts(bin)); !errors.Is(err, ErrKeptNew) {
			t.Fatalf("got %v", err)
		}
		if w.live() != "NEW" {
			t.Fatalf("live is %s", w.live())
		}
	})
	t.Run("a successful install removes it", func(t *testing.T) {
		w, bin := newSchemaWorld(t)
		stale(t, w)
		w.ready["NEW"] = true
		if _, err := Install(context.Background(), w.opts(bin)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(w.dir, failedName)); !os.IsNotExist(err) {
			t.Error("the earlier fleet.failed is still there")
		}
	})
}

// TestInstallSwapFailureIsNotNothingChanged: when the final rename fails,
// fleet.prev was already replaced (by a link to the live binary), so the
// refusal does not claim that nothing changed.
func TestInstallSwapFailureIsNotNothingChanged(t *testing.T) {
	w, bin := newSchemaWorld(t)
	failRenames(t, 1)
	_, err := Install(context.Background(), w.opts(bin))
	if err == nil || errors.Is(err, ErrNothingChanged) || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("got %v", err)
	}
	if w.live() != "OLD" || len(w.events) != 0 {
		t.Fatalf("live %s events %v", w.live(), w.events)
	}
}
