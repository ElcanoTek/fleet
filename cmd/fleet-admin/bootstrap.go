package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// cmdBootstrap wraps scripts/bootstrap.sh. It forwards --postgres / --dry-run /
// any extra flags to the shell script, which owns the idempotent role/db
// provisioning for BOTH the chat and sched databases. The CLI never runs
// migrations (services self-migrate on start).
func cmdBootstrap(argv []string) int {
	script := findBootstrapScript()
	if script == "" {
		return errf(5, "scripts/bootstrap.sh not found (run from the repo root or set FLEET_ROOT)")
	}
	args := append([]string{script}, argv...)
	// Default --postgres=local when not specified, matching the script default.
	if !hasFlag(argv, "--postgres") {
		args = append(args, "--postgres=local")
	}
	// Run under a signal-cancelled context so Ctrl-C / SIGTERM tears the
	// provisioning script down instead of orphaning it.
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
		return errf(5, "bootstrap: %v", err)
	}
	return 0
}

func findBootstrapScript() string {
	candidates := []string{}
	if root := strings.TrimSpace(os.Getenv("FLEET_ROOT")); root != "" {
		candidates = append(candidates, filepath.Join(root, "scripts", "bootstrap.sh"))
	}
	candidates = append(candidates, filepath.Join("scripts", "bootstrap.sh"))
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "scripts", "bootstrap.sh"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil { //nolint:gosec // G703: candidate paths are operator-controlled (FLEET_ROOT env, the literal "scripts/bootstrap.sh", the binary's own dir), never request or LLM input.
			return c
		}
	}
	return ""
}

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func asExit(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
