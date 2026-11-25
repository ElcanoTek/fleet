// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schema is the database's migration ledger (core.schema_versions,
// docs/MIGRATIONS.md) as the binary running the install or rollback sees it.
//
// It exists because a rollback is not only a binary swap. Every store open
// migrates, so a new release that ships a migration has usually applied it by
// the time its readiness gate fails, and the previous binary refuses a
// database holding a version it does not ship (migrations.UnknownVersionError:
// "Refusing to downgrade"). Swapping fleet.prev back in without touching the
// schema leaves systemd restarting a binary that refuses to boot. So Install
// records the versions the database holds before the restart and, when it has
// to roll back, undoes the versions that appeared since, with the down
// migrations of the binary running the install — the new release, when it is
// run as update.sh runs it. An older binary running it cannot undo versions it
// does not ship (its Down refuses them), and the new binary is then kept.
type Schema interface {
	// Applied lists the migration versions the database records, ascending
	// (empty: none, or no ledger yet).
	Applied(ctx context.Context) ([]int, error)
	// DownTo undoes every applied version above v, newest first, with this
	// binary's down migrations (each in its own transaction with its ledger
	// row), and returns the versions it undid.
	DownTo(ctx context.Context, v int) ([]int, error)
}

// schemaRecordName holds, beside fleet.prev, the migration versions the
// database held when fleet.prev was the live binary (one line, space
// separated) — what `fleet release rollback` takes the database back to. It
// describes fleet.prev, so every step that replaces fleet.prev rewrites or
// removes it.
const schemaRecordName = "fleet.prev.schema"

// ErrNothingChanged wraps a refusal that came before anything that matters
// changed: the live binary, fleet.prev and its record, and the database are as
// they were, and the service was not touched. (An install refused after
// staging may already have removed a fleet.failed an earlier failed install
// left behind.)
var ErrNothingChanged = errors.New("refused before changing anything")

// ErrMigrateDownNeeded is a manual rollback refused before anything changed:
// fleet.prev runs on an older schema than the database is at, and undoing the
// newer migrations drops what was stored in the tables and columns they added,
// so the operator has to ask for it (--migrate-down).
var ErrMigrateDownNeeded = errors.New("the previous binary runs on an older database schema")

// ErrKeptNew wraps a failed install that was NOT rolled back because the
// rollback could not safely run — the database could not be taken back to the
// schema fleet.prev runs on, or fleet.prev (or fleet.failed) could not be put
// in place with fleet stopped: the new binary stays live (it runs on the
// database in either state), and the error says why and whether it answered
// ready when it was restarted.
var ErrKeptNew = errors.New("not rolled back: the new binary was kept")

// Retry budget for reading the database's versions during a rollback: a
// database that briefly stopped answering is one of the reasons a gate fails.
var (
	schemaReadAttempts = 5
	schemaReadInterval = 3 * time.Second
)

// optionalSchemaReadTimeout bounds the read when it only feeds the record (no
// rollback armed, e.g. bootstrap's --no-restart install, where Postgres may
// not be up yet).
const optionalSchemaReadTimeout = 3 * time.Second

// readSchemaRecord returns the versions recorded for fleet.prev, if any.
func readSchemaRecord(dir string) ([]int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, schemaRecordName)) //nolint:gosec // G304: a fixed name in the validated install dir.
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(b))
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		v, err := strconv.Atoi(f)
		if err != nil || v < 0 {
			return nil, false
		}
		out = append(out, v)
	}
	sort.Ints(out)
	return out, true
}

// saveSchemaRecord records vs as the versions fleet.prev runs on, or removes
// the record when they are not known — a stale record would describe another
// binary. A failure is logged: without the record a later
// `fleet release rollback` only swaps binaries.
func (o *InstallOptions) saveSchemaRecord(vs []int, known bool) {
	path := filepath.Join(o.InstallDir, schemaRecordName)
	if !known {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			o.logf("warning: could not remove the stale %s: %v", path, err)
		}
		return
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, []byte(strings.Join(parts, " ")+"\n"), 0o644) //nolint:gosec // G306: version numbers, read by the operator's tools.
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(path)
		o.logf("warning: could not record the database versions %s runs on in %s: %v", prevName, path, err)
	}
}

// schemaBefore reads the database's versions before the new binary starts,
// when there is a live binary to keep as fleet.prev (a first install keeps
// none, so there is nothing to record). It is required when the install may
// roll back (a restart with rollback armed): without it the rollback cannot
// know what to undo, so the install refuses before anything changes.
// Otherwise it is a short, best-effort read that only feeds the record a later
// manual rollback uses (quiet with --no-restart, where Postgres may not be up).
func (o *InstallOptions) schemaBefore(ctx context.Context, keepsPrevious bool) ([]int, bool, error) {
	if o.Schema == nil || !keepsPrevious {
		return nil, false, nil
	}
	required := !o.NoRestart && !o.NoRollback
	if !required {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, optionalSchemaReadTimeout)
		defer cancel()
	}
	vs, err := o.Schema.Applied(ctx)
	switch {
	case err == nil:
		return vs, true, nil
	case required:
		return nil, false, fmt.Errorf("%w: read the database's migration versions: %w. An automatic rollback needs them to take the database back "+
			"to the schema the previous binary runs on; fix the database connection (DATABASE_URL), or install with --no-rollback", ErrNothingChanged, err)
	case !o.NoRestart:
		o.logf("warning: could not read the database's migration versions (%v); `fleet release rollback` will not know which schema %s runs on", err, prevName)
	}
	return nil, false, nil
}

// optionalApplied is schemaBefore's best-effort read for a manual rollback
// with no record: it only feeds the record of the binary that becomes
// fleet.prev.
func (o *InstallOptions) optionalApplied(ctx context.Context) ([]int, bool) {
	if o.Schema == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, optionalSchemaReadTimeout)
	defer cancel()
	vs, err := o.Schema.Applied(ctx)
	return vs, err == nil
}

// appliedWithRetry reads the database's versions, retrying a few times.
func (o *InstallOptions) appliedWithRetry(ctx context.Context) ([]int, error) {
	var err error
	for attempt := 1; ; attempt++ {
		var vs []int
		if vs, err = o.Schema.Applied(ctx); err == nil {
			return vs, nil
		}
		if attempt >= schemaReadAttempts {
			return nil, fmt.Errorf("read the database's migration versions: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("read the database's migration versions: %w (and %w)", err, ctx.Err())
		case <-time.After(schemaReadInterval):
		}
	}
}

// undoTarget works out how to take the database from versions now back to
// versions before: undo nothing when nothing appeared since; otherwise Down
// to the newest version before holds, which undoes exactly the versions that
// appeared — provided they all sort above it (Down undoes everything above
// its target). Anything else cannot be undone that way and is an error.
func undoTarget(before, now []int) (to int, need bool, err error) {
	had := make(map[int]bool, len(before))
	for _, v := range before {
		had[v] = true
	}
	var added []int
	for _, v := range now {
		if !had[v] {
			added = append(added, v)
		}
	}
	if len(added) == 0 {
		return 0, false, nil
	}
	sort.Ints(added)
	if len(before) == 0 {
		return 0, false, fmt.Errorf("the database recorded no migrations before, and down migrations cannot take it back to empty: "+
			"restore a backup taken before, or stay on the newer release (it applied %s)", joinVersions(added))
	}
	to = before[len(before)-1]
	if added[0] < to {
		return 0, false, fmt.Errorf("the versions applied since (%s) are not all above %d, the newest the database held before, so "+
			"`fleet migrate down --to %d` would not undo exactly them", joinVersions(added), to, to)
	}
	return to, true, nil
}

// stopForSchema stops the service before the schema changes: down migrations
// never run under a live fleet process (a failed release may still be booting,
// or migrating). The stop is detached from ctx's cancellation like every stop
// here, and bounded by its deadline.
func (o *InstallOptions) stopForSchema(ctx context.Context) error {
	ss, ok := o.Restarter.(StopStarter)
	if !ok {
		return errors.New("the service cannot be stopped on its own (this restarter only restarts), and the database schema is only changed with fleet stopped")
	}
	stopCtx, cancel := detachedBounded(ctx)
	defer cancel()
	if err := ss.Stop(stopCtx, o.Service); err != nil {
		return fmt.Errorf("stop %s before changing the database schema: %w", o.Service, err)
	}
	return nil
}

// undoMigrations takes the database back to the versions it held before the
// install (before), with fleet stopped: it reads the versions now (retrying)
// and, when the new binary applied any, runs this binary's down migrations to
// the newest version before held. It returns the versions it undid.
func (o *InstallOptions) undoMigrations(ctx context.Context, before []int) ([]int, error) {
	now, err := o.appliedWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	to, need, err := undoTarget(before, now)
	if err != nil {
		return nil, err
	}
	if !need {
		o.logf("the new binary did not migrate the database — nothing to undo")
		return nil, nil
	}
	o.logf("the new binary migrated the database past version %d — undoing that with down migrations so %s can run on it", to, prevName)
	undone, err := o.Schema.DownTo(ctx, to)
	if err != nil {
		return undone, fmt.Errorf("undo the migrations above version %d: %w", to, err)
	}
	o.logf("undid migration(s) %s: the database is at version %d again", joinVersions(undone), to)
	return undone, nil
}

func joinVersions(vs []int) string {
	if len(vs) == 0 {
		return "none"
	}
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}
