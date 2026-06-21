package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureWorkspaceDirIsContainerReadable pins the perms contract that
// the lockdown sandbox depends on: the per-conversation workspace dir
// must be world-readable + traversable so the in-container sandbox uid
// (1000) can chdir + read it.
//
// Regression test for the lockdown bug where EnsureWorkspaceDir created
// dirs with 0o750. Under rootless podman, host-chat maps to
// container-root, so the dir appeared as root:root 0o750 inside the
// container — and every bash/run_python call as the sandbox user died
// with EACCES on its own working directory.
func TestEnsureWorkspaceDirIsContainerReadable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	dir, err := EnsureWorkspaceDir("conv-perms-test")
	if err != nil {
		t.Fatalf("EnsureWorkspaceDir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("not a directory: %s", dir)
	}
	got := info.Mode().Perm()
	if got&0o005 != 0o005 {
		t.Errorf("perms %o lack other-read+execute; container sandbox uid 1000 won't be able to read its own workspace", got)
	}
	if got != 0o755 {
		t.Errorf("perms = %o, want 0o755 (matches production contract)", got)
	}
}

// TestEnsureWorkspaceDirChmodsPreexisting covers the upgrade path: a
// box that ran an older chat-server has per-conv dirs at 0o750 already
// on disk. EnsureWorkspaceDir's MkdirAll is a no-op on existing dirs,
// so without the explicit Chmod we'd ship a fix that only helps
// brand-new conversations and leaves existing chats broken.
func TestEnsureWorkspaceDirChmodsPreexisting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	convID := "conv-chmod-migration"
	preexisting := filepath.Join(root, convID)
	if err := os.MkdirAll(preexisting, 0o750); err != nil {
		t.Fatalf("seed preexisting: %v", err)
	}
	if err := os.Chmod(preexisting, 0o750); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}

	dir, err := EnsureWorkspaceDir(convID)
	if err != nil {
		t.Fatalf("EnsureWorkspaceDir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("preexisting 0o750 dir not migrated: perms = %o, want 0o755", got)
	}
}
