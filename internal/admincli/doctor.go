// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/boxdoctor"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// warnRetiredEnvAliases reports removed CHAT_/CUTLASS_ (or bare DATA_DIR)
// spellings (ADR-0106) and returns true when the client bundle still uses
// one. A bundle use is a FAIL: a required reference fails the load, and
// `fleet update` refuses the bundle before installing, so it must be renamed.
// A spelling still SET in the process environment, the systemd unit or the
// server env file is a WARN: nothing reads it. Read-only — the bundle is
// scanned (clientconfig.RetiredManifestRefs), never loaded — and it never
// prints a value.
func warnRetiredEnvAliases(w io.Writer) (bundleFails bool) {
	path := serverEnvFile("")
	// The bundle dir is resolved as the server sees it: on a packaged
	// install FLEET_CLIENT_CONFIG_DIR lives only in the server env file.
	bundleDir := clientconfig.DirFromEnvFile(path)
	if refs, err := clientconfig.RetiredManifestRefs(bundleDir); err != nil {
		fmt.Fprintf(w, "fleet doctor: could not check the client bundle for removed env aliases: %v\n", err)
	} else if len(refs) > 0 {
		bundleFails = true
		fmt.Fprintf(w, "FAIL the client bundle (%s) uses removed CHAT_/CUTLASS_ variables (ADR-0106); `fleet update` refuses it until each is renamed:\n", bundleDir)
		for _, r := range refs {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	findings, err := config.RetiredAliasFindings(path, loadUnitEnv())
	if err != nil {
		fmt.Fprintf(w, "fleet doctor: could not check %s for removed env aliases: %v\n", path, err)
		return bundleFails
	}
	if len(findings) == 0 {
		return bundleFails
	}
	fmt.Fprintf(w, "WARN removed CHAT_/CUTLASS_ env aliases are set (ADR-0106); nothing reads them, so rename each to its FLEET_ name (fleet config set-env / fleet env edit):\n")
	for _, f := range findings {
		fmt.Fprintf(w, "  - %s\n", f)
	}
	return bundleFails
}

// cmdDoctor wraps scripts/doctor.sh — the box-level diagnose-AND-REPAIR pass.
// It forwards every flag verbatim (--check / --no-restart / --dry-run) to the
// shell script, which owns the repairs: the runtime toolchain + fleet-critical
// package currency, the service user's rootless-podman prerequisites
// (subuid/subgid, dir ownership, containers.conf; a stale pause process is
// reported with its manual repair, never reset — podman system migrate
// deletes a live fleet's sandbox pool),
// systemd unit drift vs deploy/, env-file shape/permissions, service health +
// the /livez + /readyz probes, a sandbox smoke run as the fleet user, and the
// installed release (no host executor, web bundle, newest published release).
//
// Division of labor: `fleet status` is the quick read-only in-process report
// (safe anywhere, no root); `fleet doctor` is the deep box pass that FIXES
// drift and therefore needs root (except --check, which only diagnoses, and
// --dry-run, which prints the checklist). `fleet update` is still the only
// thing that pulls or installs — doctor reports staleness but never deploys.
//
// On a packaged/binary install with no repo checkout the script cannot be
// found; rather than failing uselessly we degrade to the in-process status
// report so `fleet doctor` is never a dead verb.
func cmdDoctor(argv []string) int {
	// Removed CHAT_/CUTLASS_ spellings (ADR-0106): reported first, on every
	// path (script or status fallback); read-only, never a value. A bundle
	// that still uses one fails the doctor run (exit 1 when the rest passed).
	bundleFails := warnRetiredEnvAliases(os.Stdout)
	warnWorktreeGit(os.Stdout)
	rc := cmdDoctorChecks(argv)
	if bundleFails && rc == 0 && !slices.Contains(argv, "--dry-run") {
		rc = 1
	}
	return rc
}

// warnWorktreeGit checks whether worktree_config tasks could run against the
// workspace repository as it is now (boxdoctor.CheckWorktreeGit, on this
// process's own record), in the workspace root resolveWorkspaceRoot names,
// and says which root it checked: a warning when they could not, one line
// when they could — the server decides from what it recorded at its own
// start — and nothing when the root is not a repository or cannot be read.
func warnWorktreeGit(w io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := resolveWorkspaceRoot("")
	// This one-off process records the workspace repository as it finds it
	// now (see worktree.Pin), then runs the check worktree tasks run.
	worktree.PinAtStartup(root)
	c := boxdoctor.CheckWorktreeGit(ctx, root)
	switch c.Status {
	case boxdoctor.StatusWarn:
		fmt.Fprintf(w, "WARN  %s (%s): %s\n", c.Name, root, c.Detail)
	case boxdoctor.StatusOK:
		fmt.Fprintf(w, "ok    %s (%s): usable as found now; the server records the repository at its own start, so a .git created or replaced after it started takes a restart%s\n", c.Name, root, doctorNotes(c.Detail))
	case boxdoctor.StatusSkip, boxdoctor.StatusFail:
		// Skip: the root is not a repository, the case on most boxes.
		// CheckWorktreeGit never fails.
	}
}

// doctorNotes is the notes boxdoctor.CheckWorktreeGit added after its ok
// detail (a shallow clone, a non-default lfs.storage), or "".
func doctorNotes(detail string) string {
	notes, _ := strings.CutPrefix(detail, boxdoctor.OKDetail)
	if notes == detail {
		return ""
	}
	return notes
}

// cmdDoctorChecks runs doctor.sh, or the status fallback without it.
func cmdDoctorChecks(argv []string) int {
	script := findScript("doctor.sh")
	if script == "" {
		fmt.Fprintln(os.Stderr, "fleet doctor: scripts/doctor.sh not found (no checkout — set FLEET_ROOT or run from the repo); falling back to the read-only `fleet status` checks.")
		// The fallback cannot honor doctor's flags, so say which ones it is
		// dropping instead of silently running a different pass — and keep
		// --dry-run's "touch nothing" promise: status's sandbox probe launches
		// a container, so it is skipped for a dry run.
		var statusArgs, dropped []string
		for _, a := range argv {
			switch a {
			case "--dry-run":
				statusArgs = append(statusArgs, "--no-sandbox")
			case "--check":
				// status is already read-only; nothing to translate.
			default:
				dropped = append(dropped, a)
			}
		}
		if len(dropped) > 0 {
			fmt.Fprintf(os.Stderr, "fleet doctor: the status fallback ignores %s (they need scripts/doctor.sh).\n", strings.Join(dropped, " "))
		}
		return cmdStatus(statusArgs)
	}
	args := append([]string{script}, argv...)
	// Run under a signal-cancelled context so Ctrl-C / SIGTERM tears the doctor
	// down instead of orphaning a half-finished dnf transaction.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	//nolint:gosec // G204: fixed "bash" binary; args are the repo-local script path + operator-supplied flags passed as separate argv (no shell string interpolation).
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return errf(5, "doctor: %v", err)
	}
	return 0
}
