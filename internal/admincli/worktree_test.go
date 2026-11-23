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
