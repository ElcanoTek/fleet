package admincli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdCleanup reclaims host-side BUILD/DEPLOY cruft — never user data. The
// motivating case is sandbox-image churn: every Containerfile change leaves
// the previous ~1.3 GB image's layers dangling in podman's overlay store, and
// a box that updates regularly fills its disk with nothing but stale layers.
//
// Scope is deliberately conservative:
//
//   - default: `podman image prune -f` (dangling layers only — an image any
//     tag still references is untouched) + the Go build/test caches when a Go
//     toolchain is present (build-from-source boxes; a binary-only box just
//     has no cache to clean).
//   - --deep: additionally `podman system prune -f` (unused NAMED images,
//     stopped containers, unused networks). While the fleet service is
//     running, its warm sandbox containers keep the current image in use, so
//     even --deep cannot remove it; on a STOPPED box --deep may remove the
//     sandbox image and the next deploy rebuilds it.
//   - --dry-run: report what would be examined without deleting anything.
//
// It never touches databases, conversation workspaces, the client-config
// checkout, or node_modules — data loss is out of scope for a cache sweep.
func cmdCleanup(argv []string) int {
	var opts cleanupOpts
	for _, a := range argv {
		switch a {
		case "--dry-run", "-n":
			opts.dryRun = true
		case "--deep":
			opts.deep = true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: fleet cleanup [--dry-run] [--deep]")
			fmt.Fprintln(os.Stderr, "  reclaim build/deploy cruft: dangling podman layers + Go build caches")
			fmt.Fprintln(os.Stderr, "  --deep also prunes unused named images / stopped containers / networks")
			fmt.Fprintln(os.Stderr, "  exit 0 when at least one prune step ran (or none applied), 5 when every step that ran failed")
			return 0
		default:
			return errf(2, "cleanup: unknown flag %q (want --dry-run and/or --deep)", a)
		}
	}
	return runCleanup(opts, defaultCleanupHost())
}

type cleanupOpts struct {
	dryRun, deep bool
}

// cleanupHost is the seam between the sweep and the box (the podman/go
// binaries, stdout), so the exit-code contract is testable without pruning
// anything. lookPath answers "is this tool installed"; run executes one step
// with its output passed through and returns its error.
type cleanupHost struct {
	lookPath func(string) (string, error)
	run      func(name string, args ...string) error
	out      io.Writer
}

func defaultCleanupHost() cleanupHost {
	return cleanupHost{lookPath: exec.LookPath, run: runLoud, out: os.Stdout}
}

// runCleanup is the whole verb behind the cleanupHost seam. Exit code: 0 when
// no prune step applied to this box (no podman, no Go toolchain, or --dry-run)
// or at least one prune step succeeded; 5 when EVERY step that ran failed.
// It used to return 0 unconditionally, so the maintenance timer's unit could
// never observe a sweep that reclaimed nothing — `systemctl status
// fleet-maintenance` said "success" over a podman store it could not open. A
// partial failure stays 0 (the failing step is reported "(continuing)"): each
// step is independent, and a box without a Go cache to clean must not page
// anyone because `go clean` had nothing to do.
func runCleanup(opts cleanupOpts, h cleanupHost) int {
	if line := diskLine("before"); line != "" {
		fmt.Fprintln(h.out, line)
	}

	attempted, failed := 0, 0
	step := func(name string, args ...string) {
		attempted++
		if err := h.run(name, args...); err != nil {
			failed++
		}
	}

	if _, err := h.lookPath("podman"); err != nil {
		fmt.Fprintln(h.out, "podman not found — skipping image cleanup.")
	} else if opts.dryRun {
		// Informational only: `podman system df` failing is not a prune failure.
		_ = h.run("podman", "system", "df")
		fmt.Fprintln(h.out, "[dry-run] would run: podman image prune -f")
		if opts.deep {
			fmt.Fprintln(h.out, "[dry-run] would run: podman system prune -f")
		}
	} else {
		fmt.Fprintln(h.out, "Pruning dangling podman image layers…")
		step("podman", "image", "prune", "-f")
		if opts.deep {
			fmt.Fprintln(h.out, "Pruning unused podman images/containers/networks (--deep)…")
			step("podman", "system", "prune", "-f")
		}
	}

	if _, err := h.lookPath("go"); err == nil {
		if opts.dryRun {
			fmt.Fprintln(h.out, "[dry-run] would run: go clean -cache -testcache")
		} else {
			fmt.Fprintln(h.out, "Cleaning the Go build/test caches…")
			step("go", "clean", "-cache", "-testcache")
		}
	}

	if !opts.dryRun {
		if line := diskLine("after"); line != "" {
			fmt.Fprintln(h.out, line)
		}
	}
	// The bundle checkout is the one tree this sweep cannot reclaim — see
	// bundle_residue.go for why it fills and why removal stays a human command.
	// Reported on a dry run too: --dry-run is what an operator uses to ask what
	// is worth reclaiming.
	reportBundleResidue(h.out)

	if attempted > 0 && failed == attempted {
		return errf(5, "cleanup: every prune step failed (%d of %d) — nothing was reclaimed", failed, attempted)
	}
	return 0
}

// runLoud runs a cleanup step with output passed through; a step failing is
// reported and returned but never aborts the sweep (each step is independent —
// runCleanup decides what the failures add up to).
func runLoud(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	//nolint:gosec // G204: name is always a fixed literal ("podman"/"go") from the call sites above; args are fixed flags — no operator or model input reaches argv.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: %s %s: %v (continuing)\n", name, strings.Join(args, " "), err)
		return err
	}
	return nil
}

// diskLine returns a one-line root-filesystem usage report, or "" when df is
// unavailable (the sweep still works; the report is a convenience).
func diskLine(label string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "df", "-h", "/").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return ""
	}
	return "disk (" + label + "): " + strings.Join(strings.Fields(lines[len(lines)-1]), " ")
}
