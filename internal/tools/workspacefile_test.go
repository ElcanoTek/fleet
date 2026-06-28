package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupWorkspace builds a temp workspace root with a couple of regular files and
// a nested dir, and returns the (symlink-resolved) root. Using EvalSymlinks on
// the root mirrors what callers do, so the containment compares are apples to
// apples even on macOS where /var → /private/var.
func setupWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	mustWrite(t, filepath.Join(resolved, "report.md"), "hello")
	if err := os.MkdirAll(filepath.Join(resolved, "data"), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	mustWrite(t, filepath.Join(resolved, "data", "raw.csv"), "a,b,c")
	mustWrite(t, filepath.Join(resolved, "my..report.csv"), "x") // legit name containing ".."
	return resolved
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSafeWorkspaceJoin_HappyPath(t *testing.T) {
	root := setupWorkspace(t)
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"top-level file", "report.md", filepath.Join(root, "report.md")},
		{"nested file", "data/raw.csv", filepath.Join(root, "data", "raw.csv")},
		{"nested dir", "data", filepath.Join(root, "data")},
		{"filename containing dotdot but not a component", "my..report.csv", filepath.Join(root, "my..report.csv")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeWorkspaceJoin(root, tc.rel)
			if err != nil {
				t.Fatalf("SafeWorkspaceJoin(%q) unexpected error: %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("SafeWorkspaceJoin(%q) = %q; want %q", tc.rel, got, tc.want)
			}
		})
	}
}

// TestSafeWorkspaceJoin_TraversalRejected is the core security test: every shape
// of "escape the workspace root" must be rejected, never returning a path.
func TestSafeWorkspaceJoin_TraversalRejected(t *testing.T) {
	root := setupWorkspace(t)
	// Place a secret OUTSIDE the workspace root (a sibling temp dir) that a
	// traversal would try to reach.
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "TOP SECRET")

	traversals := []struct {
		name    string
		rel     string
		wantErr error // nil means "any non-nil error is acceptable"
	}{
		{"parent escape", "../" + filepath.Base(outside) + "/secret.txt", ErrUnsafePath},
		{"deep parent escape", "../../etc/passwd", ErrUnsafePath},
		{"bare dotdot", "..", ErrUnsafePath},
		{"dotdot component mid-path", "data/../../" + filepath.Base(outside), ErrUnsafePath},
		{"absolute path", "/etc/passwd", ErrUnsafePath},
		{"absolute path to outside secret", filepath.Join(outside, "secret.txt"), ErrUnsafePath},
		{"NUL byte", "report\x00.md", ErrUnsafePath},
		{"empty path", "", ErrUnsafePath},
		{"backslash-prefixed absolute (windows-ish)", `\etc\passwd`, nil},
	}
	for _, tc := range traversals {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SafeWorkspaceJoin(root, tc.rel)
			if err == nil {
				t.Fatalf("SafeWorkspaceJoin(%q) returned %q with NO error; traversal must be rejected", tc.rel, got)
			}
			if got != "" {
				t.Errorf("SafeWorkspaceJoin(%q) returned non-empty path %q on error", tc.rel, got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("SafeWorkspaceJoin(%q) err = %v; want %v", tc.rel, err, tc.wantErr)
			}
			// Belt and suspenders: whatever path came back must not point at the
			// secret outside the root.
			if got != "" && strings.HasPrefix(got, outside) {
				t.Errorf("SafeWorkspaceJoin(%q) escaped to %q (outside root)", tc.rel, got)
			}
		})
	}
}

// TestSafeWorkspaceJoin_SymlinkEscapeRejected: a symlink planted INSIDE the
// workspace that points OUTSIDE it must not be followable — this is the attack
// the EvalSymlinks check exists to stop, and the syntactic guard alone cannot
// catch it (there is no ".." in the request).
func TestSafeWorkspaceJoin_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := setupWorkspace(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	mustWrite(t, secret, "TOP SECRET")

	// Symlink to a file outside the root.
	fileLink := filepath.Join(root, "escape")
	if err := os.Symlink(secret, fileLink); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	// Symlink to a directory outside the root (mirrors the structural symlinks
	// EnsureWorkspaceDir plants: an absolute target outside the workspace).
	dirLink := filepath.Join(root, "outside_dir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	for _, rel := range []string{"escape", "outside_dir/secret.txt", "outside_dir"} {
		t.Run(rel, func(t *testing.T) {
			got, err := SafeWorkspaceJoin(root, rel)
			if err == nil {
				t.Fatalf("SafeWorkspaceJoin(%q) followed symlink to %q; escape must be rejected", rel, got)
			}
			if !errors.Is(err, ErrPathEscapesWorkspace) {
				t.Errorf("SafeWorkspaceJoin(%q) err = %v; want ErrPathEscapesWorkspace", rel, err)
			}
		})
	}
}

// TestSafeWorkspaceJoin_InternalSymlinkAllowed: a symlink that stays WITHIN the
// workspace is fine — only escapes are rejected.
func TestSafeWorkspaceJoin_InternalSymlinkAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := setupWorkspace(t)
	link := filepath.Join(root, "alias.md")
	if err := os.Symlink(filepath.Join(root, "report.md"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := SafeWorkspaceJoin(root, "alias.md")
	if err != nil {
		t.Fatalf("internal symlink should resolve: %v", err)
	}
	if got != filepath.Join(root, "report.md") {
		t.Errorf("internal symlink resolved to %q; want %q", got, filepath.Join(root, "report.md"))
	}
}

func TestSafeWorkspaceJoin_NotExist(t *testing.T) {
	root := setupWorkspace(t)
	got, err := SafeWorkspaceJoin(root, "does-not-exist.md")
	if err == nil {
		t.Fatalf("missing file should error; got %q", got)
	}
	if !os.IsNotExist(err) {
		t.Errorf("missing file err = %v; want fs.ErrNotExist", err)
	}
	if got != "" {
		t.Errorf("missing file returned non-empty path %q", got)
	}
}
