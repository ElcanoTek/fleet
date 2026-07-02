package admincli

import (
	"context"
	"fmt"
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
	var dryRun, deep bool
	for _, a := range argv {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		case "--deep":
			deep = true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: fleet cleanup [--dry-run] [--deep]")
			fmt.Fprintln(os.Stderr, "  reclaim build/deploy cruft: dangling podman layers + Go build caches")
			fmt.Fprintln(os.Stderr, "  --deep also prunes unused named images / stopped containers / networks")
			return 0
		default:
			return errf(2, "cleanup: unknown flag %q (want --dry-run and/or --deep)", a)
		}
	}

	fmt.Println(diskLine("before"))

	if _, err := exec.LookPath("podman"); err != nil {
		fmt.Println("podman not found — skipping image cleanup.")
	} else {
		if dryRun {
			runLoud("podman", "system", "df")
			fmt.Println("[dry-run] would run: podman image prune -f")
			if deep {
				fmt.Println("[dry-run] would run: podman system prune -f")
			}
		} else {
			fmt.Println("Pruning dangling podman image layers…")
			runLoud("podman", "image", "prune", "-f")
			if deep {
				fmt.Println("Pruning unused podman images/containers/networks (--deep)…")
				runLoud("podman", "system", "prune", "-f")
			}
		}
	}

	if _, err := exec.LookPath("go"); err == nil {
		if dryRun {
			fmt.Println("[dry-run] would run: go clean -cache -testcache")
		} else {
			fmt.Println("Cleaning the Go build/test caches…")
			runLoud("go", "clean", "-cache", "-testcache")
		}
	}

	if !dryRun {
		fmt.Println(diskLine("after"))
	}
	return 0
}

// runLoud runs a cleanup step with output passed through; a step failing is
// reported but never aborts the sweep (each step is independent).
func runLoud(name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	//nolint:gosec // G204: name is always a fixed literal ("podman"/"go") from the call sites above; args are fixed flags — no operator or model input reaches argv.
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: %s %s: %v (continuing)\n", name, strings.Join(args, " "), err)
	}
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
