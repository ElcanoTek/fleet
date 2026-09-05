// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// timersTestDeploy writes a minimal deploy/ dir with all four shipped units so
// the install logic resolves sources without depending on the real checkout
// layout (though the repo's own deploy/ would also satisfy it).
func timersTestDeploy(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	deploy := filepath.Join(src, "deploy")
	if err := os.MkdirAll(deploy, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{
		"fleet-backup.service", "fleet-backup.timer",
		"fleet-maintenance.service", "fleet-maintenance.timer",
	} {
		if err := os.WriteFile(filepath.Join(deploy, unit), []byte("[Unit]\nDescription="+unit+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// fakeTimersHost records every mutation so the tests can assert exactly what a
// run would do to the box. installedUnits seeds the systemctl-cat probe.
type fakeTimersHost struct {
	installedUnits map[string]bool
	installs       []string // "src → dst"
	reloads        int
	enabled        []string
	dirs           []string
	root           bool
	systemd        bool
	out, errOut    bytes.Buffer
}

func (f *fakeTimersHost) host() timersHost {
	return timersHost{
		haveSystemctl: func() bool { return f.systemd },
		unitInstalled: func(unit string) bool { return f.installedUnits[unit] },
		installUnit: func(src, dst string) error {
			f.installs = append(f.installs, src+" → "+dst)
			return nil
		},
		daemonReload: func() error { f.reloads++; return nil },
		enableNow: func(timer string) error {
			f.enabled = append(f.enabled, timer)
			return nil
		},
		ensureDir: func(dir string) (bool, error) {
			f.dirs = append(f.dirs, dir)
			return true, nil
		},
		isRoot: func() bool { return f.root },
		out:    &f.out,
		errOut: &f.errOut,
	}
}

// TestTimersInstallNoSystemd — on a box without systemd (a container platform,
// Kubernetes, another supervisor) the verb must not pretend: it explains what
// to schedule instead (the two equivalent jobs) and exits non-zero.
func TestTimersInstallNoSystemd(t *testing.T) {
	f := &fakeTimersHost{systemd: false, root: true}
	if code := runTimersInstall(timersInstallOpts{}, f.host()); code == 0 {
		t.Fatal("no-systemd install returned 0; the requested install cannot have happened")
	}
	msg := f.errOut.String()
	for _, want := range []string{"no systemd", "fleet backup --db=all --prune", "fleet cleanup", "CronJob"} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-systemd message missing %q\n--- output ---\n%s", want, msg)
		}
	}
	if len(f.installs) != 0 || f.reloads != 0 || len(f.enabled) != 0 {
		t.Errorf("no-systemd run mutated the host: installs=%v reloads=%d enabled=%v", f.installs, f.reloads, f.enabled)
	}
}

// TestTimersInstallNeedsRoot — a real run without root must refuse before
// touching anything (the write target is /etc/systemd/system).
func TestTimersInstallNeedsRoot(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: false}
	code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t)}, f.host())
	if code == 0 {
		t.Fatal("unprivileged install returned 0")
	}
	if !strings.Contains(f.errOut.String(), "sudo fleet timers install") {
		t.Errorf("refusal should name the sudo re-run, got:\n%s", f.errOut.String())
	}
	if len(f.installs) != 0 || f.reloads != 0 || len(f.enabled) != 0 {
		t.Errorf("unprivileged run mutated the host: installs=%v reloads=%d enabled=%v", f.installs, f.reloads, f.enabled)
	}
}

// TestTimersInstallDryRun — the plan must print every step (unit installs, the
// backup dir, the reload+enable) and mutate nothing; it must not require root.
func TestTimersInstallDryRun(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: false, installedUnits: map[string]bool{}}
	code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t), dryRun: true}, f.host())
	if code != 0 {
		t.Fatalf("dry-run exited %d\n--- stderr ---\n%s", code, f.errOut.String())
	}
	out := f.out.String()
	for _, want := range []string{
		"would install",
		"/etc/systemd/system/fleet-backup.service",
		"/etc/systemd/system/fleet-backup.timer",
		"/etc/systemd/system/fleet-maintenance.service",
		"/etc/systemd/system/fleet-maintenance.timer",
		"would create /var/backups/fleet if missing",
		"systemctl daemon-reload && systemctl enable --now fleet-backup.timer",
		"systemctl daemon-reload && systemctl enable --now fleet-maintenance.timer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
	if len(f.installs) != 0 || f.reloads != 0 || len(f.enabled) != 0 || len(f.dirs) != 0 {
		t.Errorf("dry-run mutated the host: installs=%v reloads=%d enabled=%v dirs=%v", f.installs, f.reloads, f.enabled, f.dirs)
	}
}

// TestTimersInstallBothPairs — the default (no selector) run installs all four
// missing units, reloads systemd exactly once, enables both timers, and
// creates the backup directory.
func TestTimersInstallBothPairs(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: true, installedUnits: map[string]bool{}}
	if code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t)}, f.host()); code != 0 {
		t.Fatalf("install exited %d\n--- stderr ---\n%s", code, f.errOut.String())
	}
	if len(f.installs) != 4 {
		t.Errorf("want 4 unit installs, got %v", f.installs)
	}
	if f.reloads != 1 {
		t.Errorf("want exactly one daemon-reload, got %d", f.reloads)
	}
	if want := []string{"fleet-backup.timer", "fleet-maintenance.timer"}; strings.Join(f.enabled, ",") != strings.Join(want, ",") {
		t.Errorf("enabled = %v, want %v", f.enabled, want)
	}
	if len(f.dirs) != 1 || f.dirs[0] != "/var/backups/fleet" {
		t.Errorf("backup dir = %v, want [/var/backups/fleet]", f.dirs)
	}
}

// TestTimersInstallSelector — --maintenance must not touch the backup pair (no
// unit install, no enable, no backup directory).
func TestTimersInstallSelector(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: true, installedUnits: map[string]bool{}}
	if code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t), maintenance: true}, f.host()); code != 0 {
		t.Fatalf("install exited %d\n--- stderr ---\n%s", code, f.errOut.String())
	}
	for _, s := range f.installs {
		if strings.Contains(s, "backup") {
			t.Errorf("--maintenance installed a backup unit: %v", f.installs)
		}
	}
	if len(f.installs) != 2 || f.reloads != 1 {
		t.Errorf("want 2 installs + 1 reload, got installs=%v reloads=%d", f.installs, f.reloads)
	}
	if strings.Join(f.enabled, ",") != "fleet-maintenance.timer" {
		t.Errorf("enabled = %v, want only fleet-maintenance.timer", f.enabled)
	}
	if len(f.dirs) != 0 {
		t.Errorf("--maintenance created a backup dir: %v", f.dirs)
	}
}

// TestTimersInstallNeverOverwrites — already-installed units are left alone
// (drift reconciliation is doctor/update's consent-gated job), there is no
// pointless daemon-reload when nothing was installed, but enable --now still
// runs: it is also the repair for installed-but-disabled / enabled-but-stopped.
func TestTimersInstallNeverOverwrites(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: true, installedUnits: map[string]bool{
		"fleet-backup.service": true, "fleet-backup.timer": true,
		"fleet-maintenance.service": true, "fleet-maintenance.timer": true,
	}}
	if code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t)}, f.host()); code != 0 {
		t.Fatalf("install exited %d\n--- stderr ---\n%s", code, f.errOut.String())
	}
	if len(f.installs) != 0 {
		t.Errorf("overwrote installed units: %v", f.installs)
	}
	if f.reloads != 0 {
		t.Errorf("nothing installed but daemon-reload ran %d time(s)", f.reloads)
	}
	if len(f.enabled) != 2 {
		t.Errorf("enable --now must still run for both timers, got %v", f.enabled)
	}
	if !strings.Contains(f.out.String(), "leaving it as-is") {
		t.Errorf("output should say installed units are left alone:\n%s", f.out.String())
	}
}

// TestTimersInstallHalfPair — a half-installed pair (service present, timer
// missing) gets only its missing half installed.
func TestTimersInstallHalfPair(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: true, installedUnits: map[string]bool{
		"fleet-backup.service": true,
	}}
	if code := runTimersInstall(timersInstallOpts{src: timersTestDeploy(t), backup: true}, f.host()); code != 0 {
		t.Fatalf("install exited %d\n--- stderr ---\n%s", code, f.errOut.String())
	}
	if len(f.installs) != 1 || !strings.Contains(f.installs[0], "fleet-backup.timer") {
		t.Errorf("want only the missing timer half installed, got %v", f.installs)
	}
}

// TestTimersInstallNoCheckout — with no deploy/ dir anywhere the verb must fail
// with the pointer to FLEET_ROOT/--src, not a bare stat error.
func TestTimersInstallNoCheckout(t *testing.T) {
	f := &fakeTimersHost{systemd: true, root: true}
	code := runTimersInstall(timersInstallOpts{src: t.TempDir()}, f.host())
	if code == 0 {
		t.Fatal("install with no deploy/ returned 0")
	}
	if !strings.Contains(f.errOut.String(), "deploy") {
		t.Errorf("error should point at the deploy/ resolution, got:\n%s", f.errOut.String())
	}
}

// TestFindDeployDirRequiresUnits — a directory merely NAMED deploy must not
// match; the probe keys on the shipped fleet-backup.timer being present.
func TestFindDeployDirRequiresUnits(t *testing.T) {
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findDeployDir(empty); got != "" {
		t.Errorf("empty deploy/ matched: %q", got)
	}
	src := timersTestDeploy(t)
	if got := findDeployDir(src); got != filepath.Join(src, "deploy") {
		t.Errorf("findDeployDir(%s) = %q", src, got)
	}
}

// TestResolveBackupDir — the resolution must mirror the unit's own
// (env > env file > in-unit default) and must never accept a relative value:
// the unit runs with "/" as cwd, so a relative dir would be created under /.
func TestResolveBackupDir(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("# comment\nFLEET_BACKUP_DIR=\"/mnt/old\"\nFLEET_BACKUP_DIR='/mnt/backups'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	// resolveBackupDir now reads the file through envOrFile (shared with
	// `fleet backup`), which caches once per process.
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)

	t.Setenv("FLEET_BACKUP_DIR", "/mnt/from-env")
	if got := resolveBackupDir(); got != "/mnt/from-env" {
		t.Errorf("process env should win, got %q", got)
	}
	t.Setenv("FLEET_BACKUP_DIR", "")
	// Last assignment wins and quotes strip — the env_get contract.
	if got := resolveBackupDir(); got != "/mnt/backups" {
		t.Errorf("env file value should apply, got %q", got)
	}
	t.Setenv("FLEET_BACKUP_DIR", "relative/dir")
	if got := resolveBackupDir(); got != "/var/backups/fleet" {
		t.Errorf("relative value should fall back to the default, got %q", got)
	}
}

// TestCmdTimersUsage — the group without a subcommand (and with an unknown
// one) must print usage and exit non-zero; install must reject positionals.
func TestCmdTimersUsage(t *testing.T) {
	if code := cmdTimers(nil); code == 0 {
		t.Error("bare `fleet timers` returned 0")
	}
	if code := cmdTimers([]string{"bogus"}); code == 0 {
		t.Error("`fleet timers bogus` returned 0")
	}
	if code := cmdTimersInstall([]string{"extra"}); code == 0 {
		t.Error("`fleet timers install extra` returned 0")
	}
}
