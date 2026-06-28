package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestDefaultWorkingDir_AppliedWhenUnset verifies the per-run default working
// directory (#180 git worktree isolation seam) scopes a bash call that does not
// specify its own WorkingDir.
func TestDefaultWorkingDir_AppliedWhenUnset(t *testing.T) {
	tmp := t.TempDir()
	sb := NewHost(nil)
	defer sb.Close()
	sb.SetDefaultWorkingDir(tmp)

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "pwd"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != tmp && !strings.HasSuffix(got, tmp) {
		t.Errorf("Stdout = %q, want default working dir %q", got, tmp)
	}
}

// TestDefaultWorkingDir_RequestOverrides verifies an explicit per-call
// WorkingDir still wins over the run default — the agent can always cd elsewhere.
func TestDefaultWorkingDir_RequestOverrides(t *testing.T) {
	def := t.TempDir()
	explicit := t.TempDir()
	sb := NewHost(nil)
	defer sb.Close()
	sb.SetDefaultWorkingDir(def)

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "pwd", WorkingDir: explicit})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	got := strings.TrimSpace(string(res.Stdout))
	if got != explicit && !strings.HasSuffix(got, explicit) {
		t.Errorf("Stdout = %q, want explicit WorkingDir %q (must override default)", got, explicit)
	}
}

// TestDefaultWorkingDir_UnsetIsNoop verifies that without a default set, behavior
// is unchanged (no forced cwd) — non-worktree tasks must not regress.
func TestDefaultWorkingDir_UnsetIsNoop(t *testing.T) {
	tmp := t.TempDir()
	sb := NewHost(nil)
	defer sb.Close()
	// No SetDefaultWorkingDir call. An explicit WorkingDir still works as before.
	res, err := sb.RunBash(context.Background(), BashRequest{Command: "pwd", WorkingDir: tmp})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != tmp && !strings.HasSuffix(got, tmp) {
		t.Errorf("Stdout = %q, want %q", got, tmp)
	}
}
