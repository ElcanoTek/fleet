package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/tools"
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
