package admincli

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
	script := findScript("bootstrap.sh")
	if script == "" {
		return errf(5, "scripts/bootstrap.sh not found (run from the repo root or set FLEET_ROOT)")
	}
	args := append([]string{script}, argv...)
	// scripts/bootstrap.sh already defaults POSTGRES_MODE=local, so forward argv
	// verbatim (like cmdUpdate) rather than injecting a second default here.
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

// findScript locates a repo script by basename. It probes, in order:
// $FLEET_ROOT/scripts/<name>, ./scripts/<name>, and the binary's own
// scripts/<name>. Shared by the bootstrap and update wrappers.
func findScript(name string) string {
	candidates := []string{}
	if root := strings.TrimSpace(os.Getenv("FLEET_ROOT")); root != "" {
		candidates = append(candidates, filepath.Join(root, "scripts", name))
	}
	candidates = append(candidates, filepath.Join("scripts", name))
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "scripts", name))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil { //nolint:gosec // G703: candidate paths are operator-controlled (FLEET_ROOT env, the literal "scripts/<name>", the binary's own dir), never request or LLM input.
			return c
		}
	}
	return ""
}

func asExit(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
