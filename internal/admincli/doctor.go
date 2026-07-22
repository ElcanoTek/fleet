// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// cmdDoctor wraps scripts/doctor.sh — the box-level diagnose-AND-REPAIR pass.
// It forwards every flag verbatim (--check / --no-restart / --dry-run) to the
// shell script, which owns the repairs: toolchain floors + fleet-critical
// package currency, the service user's rootless-podman prerequisites
// (subuid/subgid, dir ownership, containers.conf, stale pause namespaces),
// systemd unit drift vs deploy/, env-file shape/permissions, service health +
// the /healthz + /readyz probes, and a sandbox smoke run as the fleet user.
//
// Division of labor: `fleet status` is the quick read-only in-process report
// (safe anywhere, no root); `fleet doctor` is the deep box pass that FIXES
// drift and therefore needs root (except --check, which only diagnoses, and
// --dry-run, which prints the checklist). `fleet update` is still the only
// thing that pulls or rebuilds — doctor reports staleness but never deploys.
//
// On a packaged/binary install with no repo checkout the script cannot be
// found; rather than failing uselessly we degrade to the in-process status
// report so `fleet doctor` is never a dead verb.
func cmdDoctor(argv []string) int {
	script := findScript("doctor.sh")
	if script == "" {
		fmt.Fprintln(os.Stderr, "fleet doctor: scripts/doctor.sh not found (no checkout — set FLEET_ROOT or run from the repo); falling back to the read-only `fleet status` checks.")
		return cmdStatus(nil)
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
