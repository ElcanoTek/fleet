package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	cmd := exec.Command("bash", args...) //nolint:gosec // script path is repo-local, args are operator-supplied flags
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
		if _, err := os.Stat(c); err == nil {
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
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	_ = fmt.Sprint // keep fmt referenced
	return false
}
