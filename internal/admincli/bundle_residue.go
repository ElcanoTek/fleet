package admincli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The client bundle checkout is the one tree on a fleet box that nothing
// reclaims. `fleet cleanup` and the daily maintenance timer sweep podman layers
// and build caches; the scheduler's sweep prunes run history and archives logs.
// None of them has ever known the bundle exists.
//
// It filled because fleet used to launch every stdio MCP server with its cwd set
// to the bundle root, so a relative output path — passed by a model as
// output_dir, or defaulted by a connector — wrote report files into the
// operator's git checkout. One box accumulated dozens of client CSV/XLSX/PDF
// files plus downloads/, reports/, sources/ and workspace/ that way. fleet now
// launches those subprocesses in a managed workspace, so this stops growing on a
// current build — but the fix removes nothing already there, and an unattended
// box would otherwise never mention it.
//
// So this REPORTS and never deletes. `fleet cleanup` runs daily and unattended
// from fleet-maintenance.timer; a daily unattended `git clean` in an operator's
// checkout would also eat any local edit, scratch file or half-finished bundle
// change they left there, which is a far worse failure than the disk it would
// save. Removal stays a human command, printed here and by `fleet doctor`.

// resolveClientBundleDir resolves the client bundle the RUNNING SERVICE loads:
// the env var fleet itself reads, else the directory bootstrap persisted under
// the state dir. Mirrors scripts/update.sh's resolution order. Empty means no
// client bundle is configured (the deployment runs the in-repo generic one).
func resolveClientBundleDir() string {
	if dir := strings.TrimSpace(os.Getenv("FLEET_CLIENT_CONFIG_DIR")); dir != "" {
		return dir
	}
	stateDir := strings.TrimSpace(os.Getenv("FLEET_STATE_DIR"))
	if stateDir == "" && repoRoot() != "" {
		stateDir = filepath.Join(repoRoot(), ".fleet-state")
	}
	if stateDir == "" {
		return ""
	}
	//nolint:gosec // G304: fixed basename under a directory that is operator config (FLEET_STATE_DIR, else the fleet checkout's own .fleet-state) — never request input. This is the file bootstrap wrote for exactly this read.
	b, err := os.ReadFile(filepath.Join(stateDir, "client-config.dir"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// bundleResidue counts the untracked files in the bundle checkout and their
// total size on disk.
//
// Measured from `git status`, not a list of directory names, so it catches
// whatever an agent actually named rather than only the shapes we happened to
// see. -uall counts the files INSIDE an untracked directory, so one reports/
// holding 300 CSVs reads as 300 rather than 1. --ignored=no is deliberate: the
// client bundles carry a .gitignore net for these same paths, and counting
// ignored files would make a tree that is still filling report clean.
//
// Returns (0, 0, false) when there is nothing to say — no bundle configured, not
// a git checkout, git unavailable, or genuinely clean.
func bundleResidue(dir string) (count int, bytes int64, ok bool) {
	if dir == "" {
		return 0, 0, false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return 0, 0, false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "git" binary; args are literal subcommands and dir is operator config (env / bootstrap state file), never request input.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "-uall", "--ignored=no").Output()
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		name := strings.TrimPrefix(line, "?? ")
		// git quotes paths containing spaces or non-ASCII; the real filenames
		// on an affected box look like
		// `RainBarrel OpenX Report_08_21_2026 08:00:25_UTC (+00:00)__x.xlsx`.
		name = strings.TrimSuffix(strings.TrimPrefix(name, `"`), `"`)
		count++
		if info, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil && !info.IsDir() {
			bytes += info.Size()
		}
	}
	return count, bytes, count > 0
}

// reportBundleResidue prints the advisory when there is residue to report.
func reportBundleResidue(w io.Writer) {
	dir := resolveClientBundleDir()
	count, bytes, ok := bundleResidue(dir)
	if !ok {
		return
	}
	fmt.Fprintf(w, "\nclient bundle checkout holds %d untracked file(s), %s (%s)\n", count, humanBytes(bytes), dir)
	fmt.Fprintln(w, "  MCP connector output written into the bundle; nothing reclaims it and it is one 'git add -A' from being committed.")
	fmt.Fprintf(w, "  review: git -C %s status --porcelain -uall\n", dir)
	fmt.Fprintf(w, "  remove: git -C %s clean -nd   (drop -n to apply)\n", dir)
}

// humanBytes renders a size the way df/du do, so the number is comparable with
// the disk lines cleanup already prints.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTP"[exp])
}
