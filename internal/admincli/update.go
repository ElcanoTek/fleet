package admincli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cmdUpdate wraps scripts/update.sh. It forwards every flag verbatim
// (--no-pull / --dry-run / --client-config / --service / --branch / --yes /
// ...) to the shell script, which owns the in-place update: pull the fleet +
// client-config checkouts, rebuild the binary + web app, rebuild the sandbox
// image only if its Containerfile changed, and restart the service. Like
// bootstrap, the CLI runs NO migrations (services self-migrate on restart).
//
// `--check` short-circuits to a READ-ONLY report (how many commits the local
// checkout is behind its upstream) and mutates nothing — handy on a dev box to
// see whether an update is even needed before running the real thing.
func cmdUpdate(argv []string) int {
	for _, a := range argv {
		if a == "--check" {
			return updateCheck()
		}
	}
	script := findScript("update.sh")
	if script == "" {
		return errf(5, "scripts/update.sh not found (run from the repo root or set FLEET_ROOT, or `make install` and run from the checkout)")
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

// updateCheck reports how many commits the local fleet checkout is behind its
// tracked upstream — read-only, no fetch-and-build, no service touch. It resolves
// the checkout from FLEET_ROOT (else the directory holding scripts/update.sh) and
// runs git there. A box that isn't a git checkout gets a clear, non-fatal note.
func updateCheck() int {
	root := repoRoot()
	if root == "" {
		fmt.Fprintln(os.Stderr, "fleet update --check: no fleet checkout found (set FLEET_ROOT or run from the repo).")
		return 5
	}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		//nolint:gosec // G204: fixed "git" binary; args are literal git subcommands, root is operator-config (FLEET_ROOT / the checkout dir), never request input.
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--is-inside-work-tree"); err != nil {
		fmt.Fprintf(os.Stderr, "fleet update --check: %s is not a git checkout (a packaged/binary install can't self-check; use your package manager).\n", root)
		return 5
	}
	branch, _ := git("rev-parse", "--abbrev-ref", "HEAD")
	// Refresh remote refs without touching the working tree, then count
	// HEAD..upstream. No upstream configured → nothing to compare against.
	if _, err := git("fetch", "--quiet"); err != nil {
		fmt.Fprintf(os.Stderr, "fleet update --check: git fetch failed (offline?): %v\n", err)
		return 5
	}
	behind, err := git("rev-list", "--count", "HEAD..@{upstream}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet update --check: no upstream tracking branch for %q — can't compare.\n", branch)
		return 5
	}
	n, _ := strconv.Atoi(behind)
	if n == 0 {
		fmt.Printf("fleet is up to date (%s, even with upstream).\n", branch)
		return 0
	}
	fmt.Printf("fleet is %d commit(s) behind upstream on %s — run `fleet update` to upgrade.\n", n, branch)
	return 0
}

// repoRoot resolves the fleet source checkout: FLEET_ROOT if set, else the parent
// of the directory holding scripts/update.sh (found via findScript). Empty when
// neither resolves (e.g. a packaged install with no checkout).
func repoRoot() string {
	if root := strings.TrimSpace(os.Getenv("FLEET_ROOT")); root != "" {
		return root
	}
	if script := findScript("update.sh"); script != "" {
		// script is <root>/scripts/update.sh → root is two levels up.
		return filepath.Dir(filepath.Dir(script))
	}
	return ""
}
