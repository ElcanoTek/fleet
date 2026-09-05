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
// --sandbox-max-age / ...) to the shell script, which owns the in-place
// update: pull the fleet + client-config checkouts, rebuild the binary + web
// app, rebuild the sandbox image when its Containerfile or resolved tag
// changed, the tag is missing from the service user's image store, or the
// installed image aged past FLEET_SANDBOX_MAX_AGE_DAYS (the freshness
// backstop: an unchanged bundle must not mean an unpatched image), and
// restart the service. Like bootstrap, the CLI runs NO migrations (services
// self-migrate on restart).
//
// `--check` short-circuits to a READ-ONLY report (how many commits the local
// checkout is behind its upstream, plus whether this box can actually build the
// web tier) and mutates nothing — handy on a dev box to see whether an update is
// even needed before running the real thing.
func cmdUpdate(argv []string) int {
	for _, a := range argv {
		if a == "--check" {
			// --check is read-only and takes no other flags; refusing the
			// extras beats silently ignoring `--client-config X` on a probe
			// that then reports on the wrong bundle.
			if len(argv) > 1 {
				return errf(1, "fleet update --check takes no other arguments (got %q)", argv)
			}
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
	} else {
		fmt.Printf("fleet is %d commit(s) behind upstream on %s — run `fleet update` to upgrade.\n", n, branch)
	}
	bundleStale := clientBundleCheck()
	if rc := nodeReadiness(); rc != 0 {
		return rc
	}
	if bundleStale {
		return 5
	}
	return 0
}

// clientBundleCheck reports the client-config bundle's freshness beside
// fleet's, and returns true when it is behind.
//
// A deployment is fleet AND its bundle: connector display names and
// descriptions, personas, protocols, skills and the MCP catalog all live in the
// bundle, so "fleet is up to date" alone answers half the operator's question.
// The two advance independently and the bundle is the half that silently does
// not — it is a separate checkout, on its own branch, that a pin or a refused
// fast-forward can hold back while fleet moves. A `--check` that cannot see it
// sends an operator looking for a fleet bug when the fix is `git -C <bundle>
// status`.
//
// The dir is resolved the way the RUNNING SERVICE resolves it — the env var
// fleet itself reads — then the bootstrap-persisted state file, mirroring
// scripts/update.sh. Read-only throughout: it fetches nothing and changes
// nothing (a `--check` that mutated a checkout would be a worse bug than the
// one it diagnoses), so it reports against the refs already on the box.
func clientBundleCheck() bool {
	dir := resolveClientBundleDir()
	if dir == "" {
		fmt.Println("client bundle: none configured (running the in-repo generic bundle).")
		return false
	}
	git := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		//nolint:gosec // G204: fixed "git" binary; args are literal git subcommands, dir is operator config (env / bootstrap state file), never request input.
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--is-inside-work-tree"); err != nil {
		fmt.Printf("client bundle at %s is not a git checkout — `fleet update` cannot advance it.\n", dir)
		return false
	}
	bundleBranch, _ := git("rev-parse", "--abbrev-ref", "HEAD")
	head, _ := git("rev-parse", "--short=12", "HEAD")
	// No upstream is the detached-HEAD / pinned case: not an error, but it does
	// mean the bundle will never fast-forward on its own, so name it.
	behind, err := git("rev-list", "--count", "HEAD..@{upstream}")
	if err != nil {
		fmt.Printf("client bundle %s at %s (%s) has no upstream tracking branch — pinned or detached; `fleet update` will not advance it.\n", bundleBranch, head, dir)
		return true
	}
	n, _ := strconv.Atoi(behind)
	if n > 0 {
		fmt.Printf("client bundle is %d commit(s) behind upstream on %s (%s) — connector names/descriptions, personas and protocols come from here.\n", n, bundleBranch, dir)
		fmt.Printf("  refresh it: git -C %s status -sb   then   sudo fleet update\n", dir)
		return true
	}
	// Current with its OWN upstream is not the same as current. A bundle parked
	// on a feature branch tracks that branch, so it reports zero commits behind
	// while sitting well behind the branch every merge actually lands on — and
	// `git pull --ff-only` succeeds, so nothing anywhere says otherwise. That is
	// the shape that hid a stale bundle behind a green update.
	if def, err := git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		def = strings.TrimPrefix(strings.TrimSpace(def), "origin/")
		if def != "" && bundleBranch != def && bundleBranch != "HEAD" {
			behindDefault, derr := git("rev-list", "--count", "HEAD..origin/"+def)
			bd, _ := strconv.Atoi(behindDefault)
			if derr == nil && bd > 0 {
				fmt.Printf("client bundle is on branch %s, not %s (%s) — current with its own branch but %d commit(s) behind %s.\n", bundleBranch, def, dir, bd, def)
				fmt.Printf("  `fleet update` will NOT fix this: it fast-forwards %s, which is not where merges land.\n", bundleBranch)
				fmt.Printf("  put it back: git -C %s checkout %s && git -C %s pull\n", dir, def, dir)
				return true
			}
			fmt.Printf("client bundle is on branch %s, not %s (%s at %s) — not behind it, so this looks deliberate.\n", bundleBranch, def, dir, head)
			return false
		}
	}
	fmt.Printf("client bundle is up to date (%s at %s, %s).\n", bundleBranch, head, dir)
	return false
}

// nodeReadiness runs the read-only node probe and returns 0 when this box can
// build the web tier, 5 when it cannot.
//
// Counting commits used to be the whole of `--check`, which made it possible for
// the command an operator runs to ask "is an update needed, and will it work?"
// to print "run `fleet update` to upgrade" and exit 0 on a box where that update
// refuses to build the web tier. Recommending a command that cannot succeed is
// the same class of fault as a plan describing work it never does.
//
// It delegates to `doctor.sh --node --check` rather than re-deriving the floor
// in Go: scripts/lib/node-version.sh is deliberately the ONE implementation of
// "which node?", and a copy here would be a fourth call site to keep in
// lockstep — exactly what that file exists to prevent. --node --check installs
// nothing and needs no root. A packaged install with no checkout has no script
// to run and no web tier to build, so the absence is not a failure.
func nodeReadiness() int {
	script := findScript("doctor.sh")
	if script == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "bash" binary; args are the repo-local script path plus literal flags, never request input.
	cmd := exec.CommandContext(ctx, "bash", script, "--node", "--check")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fleet update --check: this box cannot build the web tier yet — `sudo fleet update` repairs the node toolchain in place (or run `sudo fleet doctor --node` first).")
		return 5
	}
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
