package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveBashWorkingDir_Forced pins the #180 in-process seam: a per-run
// forced working dir takes precedence over the convID/process-cwd fallback, but
// an explicit per-call working_dir still wins.
func TestResolveBashWorkingDir_Forced(t *testing.T) {
	forced := t.TempDir()

	// No explicit request → forced dir is used (this is the worktree path for a
	// scheduled worktree run, which previously fell through to os.Getwd()).
	got, err := resolveBashWorkingDir(WithForcedWorkingDir(context.Background(), forced), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != forced {
		t.Errorf("forced dir not honored: got %q, want %q", got, forced)
	}

	// Explicit request wins over the forced dir.
	got, err = resolveBashWorkingDir(WithForcedWorkingDir(context.Background(), forced), "/explicit/dir")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/dir" {
		t.Errorf("explicit working_dir should win over forced: got %q", got)
	}

	// Absent forced dir → legacy fallback (non-empty process cwd), unchanged.
	got, err = resolveBashWorkingDir(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("expected non-empty fallback when no forced dir is set")
	}
}

// TestResolveWorkspacePath_Forced pins that the file tools resolve relative paths
// against the per-run forced working dir when there's no conversation ID (the
// scheduled worktree case), and are unchanged otherwise.
func TestResolveWorkspacePath_Forced(t *testing.T) {
	forced := t.TempDir()

	if got, err := resolveWorkspacePath(WithForcedWorkingDir(context.Background(), forced), "report.md"); err != nil || got != filepath.Join(forced, "report.md") {
		t.Errorf("relative path not scoped to forced dir: got %q, err %v", got, err)
	}
	// Absolute paths are never rewritten.
	if got, err := resolveWorkspacePath(WithForcedWorkingDir(context.Background(), forced), "/abs/p"); err != nil || got != "/abs/p" {
		t.Errorf("absolute path must pass through unchanged: got %q, err %v", got, err)
	}
	// No forced dir, no convID → unchanged (legacy behaviour).
	if got, err := resolveWorkspacePath(context.Background(), "report.md"); err != nil || got != "report.md" {
		t.Errorf("without forced dir/convID the path must be unchanged: got %q, err %v", got, err)
	}
}

// TestBashTool_ForcedWorkingDirScopesCwd drives the REAL bash tool path
// (runBash → resolveBashWorkingDir → sb.RunBash) and asserts the command runs in
// the forced dir — the end-to-end check the prior sb-only test could not make,
// because resolveBashWorkingDir always pre-fills a non-empty WorkingDir.
func TestBashTool_ForcedWorkingDirScopesCwd(t *testing.T) {
	forced := t.TempDir()
	raw, err := runBash(WithForcedWorkingDir(context.Background(), forced), BashParams{Command: "pwd"})
	if err != nil {
		t.Fatalf("runBash: %v", err)
	}
	result := parseBashResult(t, raw)
	got := strings.TrimSpace(result.Stdout)
	if got != forced && !strings.HasSuffix(got, forced) {
		t.Errorf("bash cwd = %q, want forced worktree dir %q", got, forced)
	}
}
