package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
)

// setupWorkspace builds a temp directory with a couple of regular files and a
// nested dir, and returns its (symlink-resolved) path with the directory
// opened as a WorkspaceDir.
func setupWorkspace(t *testing.T) (string, *WorkspaceDir) {
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
	d, err := OpenDirNoFollow(context.Background(), resolved)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return resolved, d
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolve_HappyPath(t *testing.T) {
	_, d := setupWorkspace(t)
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"top-level file", "report.md", "report.md"},
		{"nested file", "data/raw.csv", filepath.Join("data", "raw.csv")},
		{"nested dir", "data", "data"},
		{"the directory itself", ".", "."},
		{"filename containing dotdot but not a component", "my..report.csv", "my..report.csv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.Resolve(tc.rel)
			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q; want %q", tc.rel, got, tc.want)
			}
		})
	}
}

// Every shape of a path that leaves the directory is rejected, never returning a path.
func TestResolve_TraversalRejected(t *testing.T) {
	_, d := setupWorkspace(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "outside.txt"), "OUTSIDE")

	traversals := []struct {
		name    string
		rel     string
		wantErr error // nil means "any non-nil error is acceptable"
	}{
		{"parent", "../" + filepath.Base(outside) + "/outside.txt", ErrUnsafePath},
		{"deep parent", "../../etc/passwd", ErrUnsafePath},
		{"bare dotdot", "..", ErrUnsafePath},
		{"dotdot component mid-path", "data/../../" + filepath.Base(outside), ErrUnsafePath},
		{"interior dotdot that stays inside", "data/../report.md", ErrUnsafePath},
		{"absolute path", "/etc/passwd", ErrUnsafePath},
		{"absolute path to an outside file", filepath.Join(outside, "outside.txt"), ErrUnsafePath},
		{"NUL byte", "report\x00.md", ErrUnsafePath},
		{"empty path", "", ErrUnsafePath},
		{"backslash-prefixed absolute (windows-ish)", `\etc\passwd`, nil},
	}
	for _, tc := range traversals {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.Resolve(tc.rel)
			if err == nil {
				t.Fatalf("Resolve(%q) returned %q with NO error; traversal must be rejected", tc.rel, got)
			}
			if got != "" {
				t.Errorf("Resolve(%q) returned non-empty path %q on error", tc.rel, got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Resolve(%q) err = %v; want %v", tc.rel, err, tc.wantErr)
			}
		})
	}
}

// A symlink planted INSIDE the directory that points OUTSIDE it is not
// followable — the syntactic guard alone cannot catch it (there is no ".." in
// the request). Relative and absolute targets alike.
func TestResolve_LinkLeadingOutsideRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, d := setupWorkspace(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	mustWrite(t, outsideFile, "OUTSIDE")

	links := map[string]string{
		"outside_file": outsideFile, // absolute, to a file outside
		"outside_dir":  outside,     // absolute, to a directory outside (the shape of the seeded doc links)
		"rel_outside":  "../" + filepath.Base(outside) + "/outside.txt",
		"via_data":     "data/../../" + filepath.Base(outside),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	for _, rel := range []string{"outside_file", "outside_dir/outside.txt", "outside_dir", "rel_outside", "via_data/outside.txt"} {
		t.Run(rel, func(t *testing.T) {
			got, err := d.Resolve(rel)
			if err == nil {
				t.Fatalf("Resolve(%q) followed a symlink out of the directory to %q; it must be refused", rel, got)
			}
			if !errors.Is(err, ErrPathEscapesWorkspace) {
				t.Errorf("Resolve(%q) err = %v; want ErrPathEscapesWorkspace", rel, err)
			}
			if OutsideTarget(err) == "" {
				t.Errorf("Resolve(%q): the refusal carries no target", rel)
			}
		})
	}
	if _, err := d.Resolve("outside_dir/outside.txt"); OutsideTarget(err) != outsideFile {
		t.Errorf("outside target = %q, want %q", OutsideTarget(err), outsideFile)
	}
}

// A symlink that stays WITHIN the directory is fine — relative, or absolute
// by the directory's own host path (sandboxes see the workspace at the same
// absolute path) — and resolves to its canonical name.
func TestResolve_InternalSymlinkAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, d := setupWorkspace(t)
	for name, target := range map[string]string{
		"alias.md":  filepath.Join(root, "report.md"),
		"rel.md":    "report.md",
		"datalink":  "data",
		"nested.md": "datalink/../report.md",
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	for rel, want := range map[string]string{
		"alias.md":         "report.md",
		"rel.md":           "report.md",
		"datalink/raw.csv": filepath.Join("data", "raw.csv"),
		"nested.md":        "report.md",
	} {
		got, err := d.Resolve(rel)
		if err != nil {
			t.Fatalf("internal symlink %q should resolve: %v", rel, err)
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q; want %q", rel, got, want)
		}
	}
}

func TestResolve_NotExist(t *testing.T) {
	_, d := setupWorkspace(t)
	got, err := d.Resolve("does-not-exist.md")
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

func TestResolve_LinkLoopBounded(t *testing.T) {
	root, d := setupWorkspace(t)
	if err := os.Symlink("b", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resolve("a"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("a link loop: err = %v, want ELOOP", err)
	}
}

// OpenNoFollow refuses a link at any component, even one that stays inside.
func TestOpenNoFollow(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(ws, "out", "ok.csv"), "ok")
	outside := filepath.Join(t.TempDir(), "outside-file")
	mustWrite(t, outside, "s")
	for name, target := range map[string]string{
		"out-link.csv": outside,
		"inner.csv":    filepath.Join(ws, "out", "ok.csv"),
		"dirlink":      filepath.Join(ws, "out"),
	} {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, info, err := OpenNoFollow(root, "out/ok.csv")
	if err != nil {
		t.Fatalf("regular file: %v", err)
	}
	_ = f.Close()
	if info.Size() != 2 {
		t.Errorf("size = %d", info.Size())
	}
	for _, rel := range []string{"out-link.csv", "inner.csv", "dirlink/ok.csv", "out", "../x", "out/../out/ok.csv", "missing", "."} {
		if f, _, err := OpenNoFollow(root, rel); err == nil {
			_ = f.Close()
			t.Errorf("OpenNoFollow(%q) succeeded, want refusal", rel)
		}
	}
	if _, info, err := OpenNoFollow(root, "out"); !errors.Is(err, ErrNotRegularFile) || info == nil || !info.IsDir() {
		t.Errorf("a directory: info=%v err=%v, want ErrNotRegularFile with its info", info, err)
	}
}

// A FIFO swapped in for the file between the check and the open must not hang
// the reader: the open is non-blocking and the identity check refuses it.
// Regular files still read normally.
func TestOpenNoFollowFIFODoesNotHang(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "out.csv"), "a,b\n")
	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, _, err := OpenNoFollow(root, "out.csv")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	n, _ := f.Read(b)
	_ = f.Close()
	if string(b[:n]) != "a,b\n" {
		t.Fatalf("regular read = %q", b[:n])
	}

	openNoFollowAfterLstat = func(rel string) {
		p := filepath.Join(ws, rel)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o644); err != nil {
			t.Errorf("mkfifo: %v", err)
		}
	}
	t.Cleanup(func() { openNoFollowAfterLstat = nil })
	done := make(chan error, 1)
	go func() {
		f, _, err := OpenNoFollow(root, "out.csv")
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened as a workspace file")
		}
	case <-time.After(5 * time.Second):
		// Unblock the stuck open so the test process can exit.
		if w, err := os.OpenFile(filepath.Join(ws, "out.csv"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("open of a swapped-in FIFO blocked")
	}
}

// ── A conversation directory that is a symbolic link ──

// outsideTree is a directory outside the workspace root that a conversation
// directory link points at. Its snapshot (names, modes, content hashes) must
// be unchanged after every operation: nothing written, removed or chmodded
// there.
type outsideTree struct {
	dir string
}

const outsideMarker = "OUTSIDE-MARKER-0f3c"

func newOutsideTree(t *testing.T) outsideTree {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"marker.txt":                  outsideMarker,
		"out/report.csv":              outsideMarker,
		"attachments/keep/keep.txt":   outsideMarker,
		"user-skills/keep/SKILL.md":   outsideMarker,
		"nested/deeper/marker-2.json": outsideMarker,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 0o700: a host-side chmod to the workspace's 0o755 shows up in the
	// snapshot.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return outsideTree{dir: dir}
}

func (o outsideTree) snapshot(t *testing.T) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(o.dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(o.dir, p)
		entry := rel + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(b)
			entry += " " + hex.EncodeToString(sum[:8])
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// linkedConversation makes <root>/<convID> a symbolic link to a fresh outside
// tree and returns the tree.
func linkedConversation(t *testing.T, root, convID string) outsideTree {
	t.Helper()
	o := newOutsideTree(t)
	if err := os.Symlink(o.dir, filepath.Join(root, convID)); err != nil {
		t.Fatal(err)
	}
	return o
}

// Every host-side helper in this package that works inside a conversation's
// workspace refuses a conversation directory that is a symbolic link to a
// directory outside the workspace root, and leaves that directory exactly as
// it was: nothing read, listed, written, removed or chmodded through the link.
func TestConversationDirSymlinkRefusedByTools(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	docs := t.TempDir()
	SetSupportingDocDirs(map[string]string{"protocols": docs, "skills": docs})
	t.Cleanup(func() { SetSupportingDocDirs(nil) })

	sinks := []struct {
		name string
		run  func(c linkedCase) (output string, err error)
	}{
		{"OpenConversationDir", func(c linkedCase) (string, error) {
			d, err := OpenConversationDir(context.Background(), c.convID)
			if d != nil {
				_ = d.Close()
			}
			return "", err
		}},
		{"EnsureConversationDir (mkdir, chmod, seeded links)", func(c linkedCase) (string, error) {
			d, err := EnsureConversationDir(context.Background(), c.convID)
			if d != nil {
				_ = d.Close()
			}
			return "", err
		}},
		{"EnsureWorkspaceDir", func(c linkedCase) (string, error) {
			_, err := EnsureWorkspaceDir(c.convID)
			return "", err
		}},
		{"SeedSupportingDocSymlinks", func(c linkedCase) (string, error) {
			SeedSupportingDocSymlinks(c.convDir)
			if _, err := os.Lstat(filepath.Join(c.o.dir, "protocols")); err == nil {
				return "", nil // planted through the link: the snapshot check reports it too
			}
			return "", ErrWorkspaceDirLink
		}},
		{"bash working directory", func(c linkedCase) (string, error) {
			return resolveBashWorkingDir(WithConversationID(context.Background(), c.convID), "")
		}},
		{"file tool path", func(c linkedCase) (string, error) {
			return resolveWorkspacePath(WithConversationID(context.Background(), c.convID), "marker.txt")
		}},
		{"email content_file", func(c linkedCase) (string, error) {
			return MaterializeContentFile(c.convID, `{"content_file":"marker.txt"}`)
		}},
		{"email content_file (absolute)", func(c linkedCase) (string, error) {
			return MaterializeContentFile(c.convID, `{"content_file":"`+filepath.Join(c.convDir, "marker.txt")+`"}`)
		}},
		{"email attachment staging", func(c linkedCase) (string, error) {
			return MaterializeAttachmentPaths(c.convID, `{"attachments":[{"path":"marker.txt"}]}`)
		}},
		{"email attachment snapshot", func(c linkedCase) (string, error) {
			staging := c.t.TempDir()
			args := map[string]any{"attachments": []any{map[string]any{"path": "marker.txt"}}}
			cleanup, err := SnapshotEmailAttachments(c.convID, args, staging)
			var copied []string
			_ = filepath.WalkDir(staging, func(p string, d fs.DirEntry, _ error) error {
				if d != nil && d.Type().IsRegular() {
					b, _ := os.ReadFile(p)
					copied = append(copied, string(b))
				}
				return nil
			})
			if cleanup != nil {
				cleanup()
			}
			return strings.Join(copied, "\n"), err
		}},
		{"publish_artifact in a linked run directory", func(c linkedCase) (string, error) {
			rec := &fakeRecorder{}
			resp, err := NewPublishArtifactTool(rec, c.convDir).Run(context.Background(), fantasy.ToolCall{ID: "1", Input: `{"path":"marker.txt"}`})
			if err != nil {
				return "", err
			}
			if len(rec.calls) > 0 || !resp.IsError {
				return resp.Content, nil
			}
			return resp.Content, ErrWorkspaceDirLink
		}},
	}
	// Each sink gets a conversation of its own whose directory is a link to a
	// fresh outside tree, so every refusal is checked on its own.
	for i, sk := range sinks {
		t.Run(sk.name, func(t *testing.T) {
			convID := fmt.Sprintf("conv-linked-%d", i)
			o := linkedConversation(t, root, convID)
			before := o.snapshot(t)
			convDir := filepath.Join(root, convID)
			out, err := sk.run(linkedCase{t: t, convID: convID, convDir: convDir, o: o})
			if err == nil {
				t.Errorf("%s: not refused (output %q)", sk.name, out)
			}
			if strings.Contains(out, outsideMarker) {
				t.Errorf("%s: read the outside directory: %q", sk.name, out)
			}
			if after := o.snapshot(t); after != before {
				t.Fatalf("%s changed the outside directory:\nbefore\n%s\nafter\n%s", sk.name, before, after)
			}
			// The link itself is still a link: nothing replaced it with a
			// directory (which would hide the condition) or removed it.
			if info, err := os.Lstat(convDir); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("%s: conversation link: %v %v", sk.name, info, err)
			}
		})
	}
}

// linkedCase is one sink's conversation: its id, its directory path (a link to
// o.dir) and the outside tree.
type linkedCase struct {
	t       *testing.T
	convID  string
	convDir string
	o       outsideTree
}

// A link to ANOTHER directory inside the workspace (another conversation) is
// refused the same way: the conversation level is never followed at all.
func TestOpenConversationDirRefusesLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "conv-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "conv-b", "b.txt"), "b")
	if err := os.Symlink("conv-b", filepath.Join(root, "conv-a")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "conv-file"), "x")
	for _, id := range []string{"conv-a", "conv-file"} {
		if d, err := OpenConversationDirIn(context.Background(), root, id); !errors.Is(err, ErrWorkspaceDirLink) {
			if d != nil {
				_ = d.Close()
			}
			t.Errorf("%s: err = %v, want ErrWorkspaceDirLink", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", "/abs"} {
		if _, err := OpenConversationDirIn(context.Background(), root, id); !errors.Is(err, ErrNotConversationID) {
			t.Errorf("id %q: err = %v, want ErrNotConversationID", id, err)
		}
	}
	d, err := OpenConversationDirIn(context.Background(), root, "conv-b")
	if err != nil {
		t.Fatalf("a real conversation directory: %v", err)
	}
	_ = d.Close()
}

// The conversation directory is replaced by a link between its check and its
// open: the open is refused (the opened directory is not the one checked), for
// a link out of the root and a link to another directory inside it.
func TestOpenConversationDirRefusesSwapBetweenCheckAndOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(t *testing.T, root string) string
	}{
		{"link out of the workspace root", func(t *testing.T, _ string) string { return newOutsideTree(t).dir }},
		{"link to another conversation", func(t *testing.T, root string) string {
			if err := os.Mkdir(filepath.Join(root, "conv-other"), 0o755); err != nil {
				t.Fatal(err)
			}
			return "conv-other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			const convID = "conv-swap"
			conv := filepath.Join(root, convID)
			if err := os.Mkdir(conv, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target(t, root)
			openChildAfterLstat = func(name string) {
				if name != convID {
					return
				}
				if err := os.Rename(conv, conv+".moved"); err != nil {
					t.Errorf("rename: %v", err)
				}
				if err := os.Symlink(target, conv); err != nil {
					t.Errorf("symlink: %v", err)
				}
			}
			t.Cleanup(func() { openChildAfterLstat = nil })
			d, err := OpenConversationDirIn(context.Background(), root, convID)
			if d != nil {
				_ = d.Close()
			}
			if err == nil {
				t.Fatal("a directory swapped for a link between check and open was opened")
			}
		})
	}
}

// Once opened, work stays in the opened directory: a link swapped in for the
// conversation directory afterwards (mid-operation) is not followed by reads,
// writes, listings or mode changes made through the WorkspaceDir.
func TestWorkspaceDirIgnoresLinkSwappedInAfterOpen(t *testing.T) {
	root := t.TempDir()
	const convID = "conv-after"
	conv := filepath.Join(root, convID)
	if err := os.Mkdir(conv, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(conv, "mine.txt"), "mine")
	d, err := OpenConversationDirIn(context.Background(), root, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	o := newOutsideTree(t)
	before := o.snapshot(t)
	if err := os.Rename(conv, conv+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.dir, conv); err != nil {
		t.Fatal(err)
	}

	if err := d.WriteFile("written.txt", []byte("w"), 0o644); err != nil {
		t.Fatalf("write through the opened directory: %v", err)
	}
	if err := d.Chmod(".", 0o755); err != nil {
		t.Fatalf("chmod through the opened directory: %v", err)
	}
	entries, err := fs.ReadDir(d.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "marker.txt" {
			t.Fatal("listing followed the link swapped in after the open")
		}
	}
	if _, _, _, err := d.OpenResolved("marker.txt"); err == nil {
		t.Fatal("read followed the link swapped in after the open")
	}
	f, _, _, err := d.OpenResolved("mine.txt")
	if err != nil {
		t.Fatalf("the opened directory's own file: %v", err)
	}
	_ = f.Close()
	if _, err := os.Stat(filepath.Join(conv+".moved", "written.txt")); err != nil {
		t.Fatalf("the write did not land in the opened directory: %v", err)
	}
	if after := o.snapshot(t); after != before {
		t.Fatalf("the outside directory changed:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// OpenWorkspaceDir refuses a link at ANY level below the root — a scheduled
// worktree directory whose parent was replaced, not just the last level.
func TestOpenWorkspaceDirRefusesLinkAtEveryLevel(t *testing.T) {
	root := t.TempDir()
	o := newOutsideTree(t)
	if err := os.MkdirAll(filepath.Join(o.dir, "run-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.dir, filepath.Join(root, ".fleet-worktrees")); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(root, ".fleet-worktrees", "run-1")
	if d, err := OpenWorkspaceDir(context.Background(), root, run); !errors.Is(err, ErrWorkspaceDirLink) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("err = %v, want ErrWorkspaceDirLink", err)
	}
	if d, err := OpenRunDir(context.Background(), root, run); !errors.Is(err, ErrWorkspaceDirLink) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("OpenRunDir err = %v, want ErrWorkspaceDirLink", err)
	}
	if _, err := OpenWorkspaceDir(context.Background(), root, t.TempDir()); err == nil {
		t.Fatal("a directory outside the root was opened as a workspace directory")
	}
	d, err := OpenWorkspaceDir(context.Background(), root, root)
	if err != nil {
		t.Fatalf("the root itself: %v", err)
	}
	_ = d.Close()
}

// OpenRunDir recognises a directory spelled through the workspace root's
// resolved path as one under the root, so it still gets the no-follow open
// instead of the as-given open meant for directories outside the root.
func TestOpenRunDirRecognisesTheResolvedRootSpelling(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	o := newOutsideTree(t)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.dir, filepath.Join(target, "run")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(link, "run"), filepath.Join(resolved, "run")} {
		if d, err := OpenRunDir(context.Background(), link, dir); !errors.Is(err, ErrWorkspaceDirLink) {
			if d != nil {
				_ = d.Close()
			}
			t.Errorf("OpenRunDir(%q) err = %v, want ErrWorkspaceDirLink", dir, err)
		}
	}
}

// A symlinked workspace ROOT is operator configuration and is followed; an
// absolute in-workspace link spelled through either path resolves.
func TestOpenConversationDirFollowsSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "conv"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(target, "conv", "a.txt"), "a")
	if err := os.Symlink(filepath.Join(link, "conv", "a.txt"), filepath.Join(target, "conv", "via-link")); err != nil {
		t.Fatal(err)
	}
	resolvedReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(resolvedReal, "conv", "a.txt"), filepath.Join(target, "conv", "via-real")); err != nil {
		t.Fatal(err)
	}
	d, err := OpenConversationDirIn(context.Background(), link, "conv")
	if err != nil {
		t.Fatalf("open under a symlinked root: %v", err)
	}
	defer d.Close()
	for _, rel := range []string{"via-link", "via-real"} {
		if got, err := d.Resolve(rel); err != nil || got != "a.txt" {
			t.Errorf("Resolve(%q) = %q, %v; want a.txt", rel, got, err)
		}
	}
}

// A FIFO swapped in for a directory between its Lstat and its open must not
// hang the opener: a FIFO with no writer would block an open forever, pinning
// an OS thread no context can cancel. Both the conversation level and an
// intermediate level of OpenNoFollow are refused promptly.
func TestOpenerRefusesFIFOSwappedInForADirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level string // the directory name the seam swaps for a FIFO
		open  func(root string) error
	}{
		{"conversation directory", "conv-fifo", func(root string) error {
			d, err := OpenConversationDirIn(context.Background(), root, "conv-fifo")
			if d != nil {
				_ = d.Close()
			}
			return err
		}},
		{"intermediate directory of OpenNoFollow", "out", func(root string) error {
			r, err := os.OpenRoot(filepath.Join(root, "conv-fifo"))
			if err != nil {
				return err
			}
			defer r.Close()
			f, _, err := OpenNoFollow(r, "out/a.txt")
			if f != nil {
				_ = f.Close()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			conv := filepath.Join(root, "conv-fifo")
			if err := os.MkdirAll(filepath.Join(conv, "out"), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(conv, "out", "a.txt"), "a")
			var fifo string
			openChildAfterLstat = func(name string) {
				if name != tc.level {
					return
				}
				dir := conv
				if tc.level == "out" {
					dir = filepath.Join(conv, "out")
				}
				if err := os.Rename(dir, dir+".moved"); err != nil {
					t.Errorf("rename: %v", err)
					return
				}
				if err := syscall.Mkfifo(dir, 0o644); err != nil {
					t.Errorf("mkfifo: %v", err)
				}
				fifo = dir
			}
			t.Cleanup(func() { openChildAfterLstat = nil })
			done := make(chan error, 1)
			go func() { done <- tc.open(root) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a FIFO swapped in for a directory was opened")
				}
				if tc.level == "conv-fifo" && !errors.Is(err, ErrWorkspaceDirLink) {
					t.Errorf("err = %v, want ErrWorkspaceDirLink", err)
				}
			case <-time.After(5 * time.Second):
				// Unblock the stuck open so the test process can exit.
				if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
				}
				t.Fatal("the open of a FIFO swapped in for a directory blocked")
			}
		})
	}
}

// WalkNoFollow lists through descriptors: a directory link is reported but
// never descended, a name that is not UTF-8 is walked, SkipDir skips a
// subtree, and an unreadable directory is reported to fn with its error.
func TestWalkNoFollow(t *testing.T) {
	root := t.TempDir()
	odd := "odd-\xff"
	for _, d := range []string{filepath.Join("a", odd), "skip/inner", "locked"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "a", odd, "f.txt"), "f")
	mustWrite(t, filepath.Join(root, "skip", "inner", "g.txt"), "g")
	if err := os.Symlink(newOutsideTree(t).dir, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o755) })
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var seen []string
	var failed []string
	err = WalkNoFollow(r, func(rel string, _ fs.DirEntry, _ *os.Root, err error) error {
		if err != nil {
			failed = append(failed, rel)
			return nil //nolint:nilerr // record the unreadable directory and keep walking
		}
		seen = append(seen, rel)
		if rel == "skip" {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)
	want := []string{"a", filepath.Join("a", odd), filepath.Join("a", odd, "f.txt"), "link", "locked", "skip"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Errorf("walked %q, want %q", seen, want)
	}
	if strings.Join(failed, "|") != "locked" {
		t.Errorf("errors reported for %q, want the unreadable directory", failed)
	}
}
