package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// cmdUpdate wraps scripts/update.sh. It forwards every flag verbatim
// (--no-pull / --dry-run / --client-config / --service / --branch / --yes /
// ...) to the shell script, which owns the in-place update: pull the fleet +
// client-config checkouts, rebuild the binary + web app, rebuild the sandbox
// image only if its Containerfile changed, and restart the service. Like
// bootstrap, the CLI runs NO migrations (services self-migrate on restart).
func cmdUpdate(argv []string) int {
	script := findScript("update.sh")
	if script == "" {
		return errf(5, "scripts/update.sh not found (run from the repo root or set FLEET_ROOT)")
	}
	args := append([]string{script}, argv...)
	// Run under a signal-cancelled context so Ctrl-C / SIGTERM tears the update
	// down instead of orphaning a half-finished build.
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
		return errf(5, "update: %v", err)
	}
	return 0
}
