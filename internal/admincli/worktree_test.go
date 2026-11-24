package admincli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/worktree"
)

// TestWorktreePrunePrintsALockReasonSafely: a lock reason is text a run can
// write; `fleet worktree prune` prints it with control characters replaced.
func TestWorktreePrunePrintsALockReasonSafely(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t.t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	worktree.PinAtStartup(root)
	if err := worktree.PrepareRepoAtStartup(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, worktree.Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := worktree.CreateWorktree(context.Background(), root, "task-run", "fleet/task-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "locked"), []byte("keep \x1b]0;title\x07 \x1b[31mred\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * worktree.DefaultPruneAge)
	if err := os.Chtimes(filepath.Join(root, worktree.Subdir, "task-run"), old, old); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStderr(t, func() { code = worktreePrune([]string{"--workspace", root}) })
	if code != 0 {
		t.Fatalf("worktree prune = %d: %s", code, out)
	}
	if !strings.Contains(out, "locked (keep") || strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("prune output %q: want the lock reason with control characters replaced", out)
	}
}

// TestResolveWorkspaceRootLikeTheService: without --workspace, the root is
// FLEET_WORKSPACE_ROOT from the environment, else from the deployment env
// file, else ./workspace; a relative value is taken against the service home
// when it exists, else the working directory. An unreadable env file is
// skipped.
func TestResolveWorkspaceRootLikeTheService(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", "")
	home := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	resolve := func(t *testing.T, envFile, body string, mode os.FileMode) string {
		t.Helper()
		if body != "" {
			if err := os.WriteFile(envFile, []byte(body), mode); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("FLEET_ENV_FILE", envFile)
		resetEnvFileCache()
		t.Cleanup(resetEnvFileCache)
		return resolveWorkspaceRoot("")
	}

	t.Run("env file readable", func(t *testing.T) {
		t.Setenv("FLEET_SERVICE_HOME", home)
		dir := t.TempDir()
		if got := resolve(t, filepath.Join(dir, "abs.env"), "FLEET_WORKSPACE_ROOT=/srv/fleet-ws\n", 0o600); got != "/srv/fleet-ws" {
			t.Errorf("absolute value from the env file = %q", got)
		}
		if got, want := resolve(t, filepath.Join(dir, "rel.env"), "FLEET_WORKSPACE_ROOT=ws\n", 0o600), filepath.Join(home, "ws"); got != want {
			t.Errorf("relative value from the env file = %q, want %q", got, want)
		}
	})
	t.Run("env file unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		t.Setenv("FLEET_SERVICE_HOME", home)
		if got, want := resolve(t, filepath.Join(t.TempDir(), "fleet.env"), "FLEET_WORKSPACE_ROOT=/srv/fleet-ws\n", 0o000), filepath.Join(home, "workspace"); got != want {
			t.Errorf("unreadable env file = %q, want the service home's %q", got, want)
		}
	})
	t.Run("neither", func(t *testing.T) {
		t.Setenv("FLEET_SERVICE_HOME", filepath.Join(t.TempDir(), "absent"))
		got := resolve(t, filepath.Join(t.TempDir(), "absent.env"), "", 0)
		if filepath.Base(got) != "workspace" || mustEvalPath(t, filepath.Dir(got)) != mustEvalPath(t, cwd) {
			t.Errorf("no env file, no service home = %q, want ./workspace under %q", got, cwd)
		}
	})
	t.Run("environment wins", func(t *testing.T) {
		t.Setenv("FLEET_SERVICE_HOME", home)
		t.Setenv("FLEET_WORKSPACE_ROOT", "/from/env")
		if got := resolve(t, filepath.Join(t.TempDir(), "fleet.env"), "FLEET_WORKSPACE_ROOT=/srv/fleet-ws\n", 0o600); got != "/from/env" {
			t.Errorf("process env = %q, want /from/env", got)
		}
	})
	if got := resolveWorkspaceRoot("/explicit"); got != "/explicit" {
		t.Errorf("--workspace = %q", got)
	}
}

// mustEvalPath is filepath.EvalSymlinks or a fatal error.
func mustEvalPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
