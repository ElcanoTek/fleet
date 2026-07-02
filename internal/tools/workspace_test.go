package tools

import (
	"context"
	"errors"
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

// TestResolveWorkspacePathRejectsTraversal pins the #575 fix: with a
// conversation id in context, a relative path carrying a ".." component is
// rejected with a *PathSecurityError instead of being joined —
// filepath.Join collapses "..", so "../<otherConvID>/file" would otherwise
// resolve into a SIBLING conversation's workspace (still under cwd) and
// sail through ValidatePath. The workspace is a cross-user isolation
// boundary; the reject has to happen on the raw input.
func TestResolveWorkspacePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	ctx := WithConversationID(context.Background(), "conv-isolation-a")

	for _, path := range []string{
		"../conv-isolation-b/secret.txt",
		"..",
		"sub/../../conv-isolation-b/secret.txt",
		"../../../../etc/passwd",
	} {
		got, err := resolveWorkspacePath(ctx, path)
		if err == nil {
			t.Errorf("resolveWorkspacePath(%q) = %q with NO error; '..' must be rejected", path, got)
			continue
		}
		var pse *PathSecurityError
		if !errors.As(err, &pse) {
			t.Errorf("resolveWorkspacePath(%q): want *PathSecurityError, got %T: %v", path, err, err)
		}
	}

	// Legitimate shapes keep working: a nested relative path lands under the
	// conversation's own workspace, and a ".."-free dotted filename is not
	// falsely rejected.
	for _, path := range []string{"sub/file.txt", "my..report.csv"} {
		got, err := resolveWorkspacePath(ctx, path)
		if err != nil {
			t.Fatalf("resolveWorkspacePath(%q): %v", path, err)
		}
		want := filepath.Join(root, "conv-isolation-a", path)
		if got != want {
			t.Errorf("resolveWorkspacePath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestResolveWorkspacePathNoConvIDUnchanged pins that the no-conversation
// paths (scheduled runs, direct invocations) keep their legacy behavior:
// unscoped passthrough, or a join against the forced working dir when git
// worktree isolation (#180) set one.
func TestResolveWorkspacePathNoConvIDUnchanged(t *testing.T) {
	got, err := resolveWorkspacePath(context.Background(), "some/relative.txt")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if got != "some/relative.txt" {
		t.Errorf("passthrough = %q, want unchanged", got)
	}

	forced := t.TempDir()
	ctx := WithForcedWorkingDir(context.Background(), forced)
	got, err = resolveWorkspacePath(ctx, "out/report.md")
	if err != nil {
		t.Fatalf("forced dir: %v", err)
	}
	if want := filepath.Join(forced, "out/report.md"); got != want {
		t.Errorf("forced dir = %q, want %q", got, want)
	}

	// Absolute paths always pass through untouched (containment is
	// ValidatePath's job for those).
	got, err = resolveWorkspacePath(WithConversationID(context.Background(), "conv-x"), "/tmp/abs.txt")
	if err != nil {
		t.Fatalf("absolute: %v", err)
	}
	if got != "/tmp/abs.txt" {
		t.Errorf("absolute = %q, want unchanged", got)
	}
}

// TestWorkspaceDirForConversation_ConfinesID pins the id-side half of the
// path-confinement invariant (the relPath side lives in SafeWorkspaceJoin): a
// conversation id may only ever name a lexically-local segment under the
// workspace root. A non-local id (absolute, "..", or an escape) must fall back to
// the shared root rather than widen the resolved path, while a normal
// uuid-shaped id joins as-is.
func TestWorkspaceDirForConversation_ConfinesID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	for _, id := range []string{"", "..", "../evil", "/etc", "sub/../../x"} {
		if got := WorkspaceDirForConversation(id); got != root {
			t.Errorf("WorkspaceDirForConversation(%q) = %q; a non-local id must fall back to the shared root %q", id, got, root)
		}
	}

	const okID = "550e8400-e29b-41d4-a716-446655440000"
	if got, want := WorkspaceDirForConversation(okID), filepath.Join(root, okID); got != want {
		t.Errorf("WorkspaceDirForConversation(%q) = %q; want %q", okID, got, want)
	}
}
