package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/worktree"
)

// TestEnsureMCPRunsDirMountedReadOnly pins Codex P1 on #1709: a scheduled
// task's MCP run dir lives in the workspace tree every sandbox mounts
// read-write, and the host-side connector writes its ledger there by pathname
// for the whole run, so validating the dir once at setup cannot stop a
// sandbox from swapping it for a symlink afterwards. The pool therefore
// creates mcp-runs/ before any sandbox exists and puts it in the read-only
// mount set — nested in the workspace root, so podman overlays it `:ro` and
// kubernetes mounts it as a read-only subPath of the claim.
func TestEnsureMCPRunsDirMountedReadOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	dir, err := ensureMountBaseDir(agentcore.PerRunMCPWorkspaceBase(root), 0o750, "MCP run dir base")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("mcp-runs must exist before any sandbox starts: %v, %v", fi, err)
	}
	// It is exactly the parent of the run dirs the spawn path hands out.
	runDir, run, err := agentcore.OpenStableMCPWorkspace(t.Context(), "task-abc")
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	if filepath.Dir(runDir) != dir {
		t.Fatalf("read-only mount %q does not cover run dir %q", dir, runDir)
	}
	// Nested in the workspace root: the shape both backends mount read-only
	// over the read-write workspace mount.
	nested, others := splitWorkspaceNestedMounts([]string{dir}, root)
	if len(nested) != 1 || len(others) != 0 {
		t.Fatalf("mcp-runs must nest in the workspace root: nested=%v others=%v", nested, others)
	}
}

// A symlink squatting on mcp-runs fails boot instead of being mounted (which
// would overlay its target into every sandbox).
func TestEnsureMCPRunsDirRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "mcp-runs")); err != nil {
		t.Fatal(err)
	}
	if dir, err := ensureMountBaseDir(agentcore.PerRunMCPWorkspaceBase(root), 0o750, "MCP run dir base"); err == nil {
		t.Fatalf("expected a symlinked mcp-runs to be refused, got %q", dir)
	}
}

// The per-job scheduled workspaces tree (#1897): created before any sandbox
// exists, at exactly the parent of every job directory, and a symlink
// squatting on the name fails boot rather than being mounted.
func TestEnsureTaskWorkspacesDir(t *testing.T) {
	root := t.TempDir()
	dir, err := ensureMountBaseDir(filepath.Join(root, tools.TaskWorkspacesDirName), 0o755, "scheduled workspaces dir")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("tasks/ must exist before any sandbox starts: %v, %v", fi, err)
	}
	if job := tools.TaskWorkspaceDir(root, "11111111-1111-1111-1111-111111111111"); filepath.Dir(job) != dir {
		t.Fatalf("read-only mount %q does not cover job dir %q", dir, job)
	}
	if again, err := ensureMountBaseDir(filepath.Join(root, tools.TaskWorkspacesDirName), 0o755, "scheduled workspaces dir"); err != nil || again != dir {
		t.Fatalf("second boot: %q, %v; want %q, nil", again, err, dir)
	}

	squatted := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(squatted, "tasks")); err != nil {
		t.Fatal(err)
	}
	if dir, err := ensureMountBaseDir(filepath.Join(squatted, tools.TaskWorkspacesDirName), 0o755, "scheduled workspaces dir"); err == nil {
		t.Fatalf("expected a symlinked tasks/ to be refused, got %q", dir)
	}
}

// The shared MCP workspace (mcp-shared/) is created before any sandbox
// exists, is exactly the directory the shared spawns hand out as
// ${FLEET_WORKSPACE}, and a symlink squatting on it fails boot.
func TestEnsureSharedMCPWorkspaceDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	dir, err := ensureMountBaseDir(agentcore.SharedMCPWorkspaceBase(root), 0o750, "shared MCP workspace dir")
	if err != nil {
		t.Fatal(err)
	}
	if got := agentcore.SharedMCPWorkspaceDir(t.Context()); got != dir {
		t.Fatalf("read-only mount %q is not the shared MCP workspace %q", dir, got)
	}
	squatted := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(squatted, "mcp-shared")); err != nil {
		t.Fatal(err)
	}
	if dir, err := ensureMountBaseDir(agentcore.SharedMCPWorkspaceBase(squatted), 0o750, "shared MCP workspace dir"); err == nil {
		t.Fatalf("expected a symlinked mcp-shared to be refused, got %q", dir)
	}
}

// wholeRootGitDirs decides at boot which of the root's git directories every
// whole-root sandbox mounts as its own (read-only, outside worktree runs)
// bind mount.
func TestWholeRootGitDirs(t *testing.T) {
	t.Run("a root that is not a repository gets none, and nothing is created", func(t *testing.T) {
		root := t.TempDir()
		dirs, err := wholeRootGitDirs(root)
		if err != nil || len(dirs) != 0 {
			t.Fatalf("dirs = %v, err = %v; want none", dirs, err)
		}
		if _, err := os.Lstat(filepath.Join(root, worktree.Subdir)); !os.IsNotExist(err) {
			t.Fatalf(".fleet-worktrees created in a non-repository root: %v", err)
		}
	})
	t.Run("a repository root gets .git and a created .fleet-worktrees", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		dirs, err := wholeRootGitDirs(root)
		want := []string{filepath.Join(root, ".git"), filepath.Join(root, worktree.Subdir)}
		if err != nil || !slices.Equal(dirs, want) {
			t.Fatalf("dirs = %v, err = %v; want %v", dirs, err, want)
		}
		if !worktree.Pin(root).IsRepo() {
			t.Fatal("the repository was not recorded at startup")
		}
		if fi, err := os.Lstat(want[1]); err != nil || !fi.IsDir() {
			t.Fatalf(".fleet-worktrees must exist before any sandbox starts: %v", err)
		}
	})
	t.Run("a gitfile or a symlinked .git gets nothing, and nothing is created", func(t *testing.T) {
		for name, mk := range map[string]func(string) error{
			"gitfile": func(p string) error { return os.WriteFile(p, []byte("gitdir: /elsewhere\n"), 0o644) },
			"symlink": func(p string) error { return os.Symlink(t.TempDir(), p) },
		} {
			root := t.TempDir()
			if err := mk(filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			dirs, err := wholeRootGitDirs(root)
			if err != nil || len(dirs) != 0 {
				t.Errorf("%s: dirs = %v, err = %v; want none", name, dirs, err)
			}
			if _, err := os.Lstat(filepath.Join(root, worktree.Subdir)); !os.IsNotExist(err) {
				t.Errorf("%s: .fleet-worktrees created: %v", name, err)
			}
			if worktree.Pin(root).IsRepo() {
				t.Errorf("%s: recorded as a repository at startup", name)
			}
		}
	})
	t.Run("a symlink squatting on .fleet-worktrees fails boot", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(root, worktree.Subdir)); err != nil {
			t.Fatal(err)
		}
		if dirs, err := wholeRootGitDirs(root); err == nil {
			t.Fatalf("expected a symlinked .fleet-worktrees to be refused, got %v", dirs)
		}
	})
}
