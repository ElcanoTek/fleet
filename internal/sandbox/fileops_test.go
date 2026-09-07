package sandbox

// #784: the sandboxed file-operation seam. These tests drive the REAL embedded
// fileops.py through the host backend (python3 subprocess, no podman) so the
// executor's read/write/edit semantics — offset/limit, atomic new-file 0600 +
// existing-mode preservation, unique/all replace, and typed errors — are
// verified on every platform. The full in-container routing (runtime, seccomp,
// caps, lockdown network) is covered by the podman-gated test below.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fileopTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fileop seam")
	}
	sb := NewHost(nil)
	t.Cleanup(sb.Close)
	return sb
}

func TestFileOp_ReadWriteEditRoundtrip(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	// Write creates nested dirs, atomic, 0600.
	wr, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: dir, Data: []byte("hello world")})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if wr.Size != 11 {
		t.Errorf("write size = %d, want 11", wr.Size)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat nested dir: %v", err)
	} else if info.Mode().Perm() != 0o750 {
		t.Errorf("nested dir mode = %o, want 750", info.Mode().Perm())
	}

	// Read back, full + offset/limit.
	rd, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: dir})
	if err != nil || string(rd.Data) != "hello world" || rd.Size != 11 {
		t.Fatalf("read = %q size %d err %v", rd.Data, rd.Size, err)
	}
	rd, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: dir, Offset: 6, Limit: 3})
	if err != nil || string(rd.Data) != "wor" {
		t.Fatalf("read offset/limit = %q err %v", rd.Data, err)
	}
	if rd.Size != 11 {
		t.Errorf("read reports total size %d, want 11", rd.Size)
	}

	// Overwrite/edit preserve metadata; a unique edit returns hashes + diff.
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: dir, Data: []byte("x A y\n")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("overwrite mode = %o, want 0751", info.Mode().Perm())
	}
	ed, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: dir, OldText: "A", NewText: "B"})
	if err != nil || ed.ReplacedCount != 1 {
		t.Fatalf("unique edit = count %d err %v", ed.ReplacedCount, err)
	}
	if ed.SHA256 == "" || ed.OldSHA256 == "" || ed.Diff == "" {
		t.Errorf("edit result missing hashes/diff: %+v", ed)
	}
	if got, _ := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: dir}); string(got.Data) != "x B y\n" {
		t.Errorf("after unique edit = %q, want 'x B y\\n'", got.Data)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: dir, Data: []byte("a a a")}); err != nil {
		t.Fatal(err)
	}
	ed, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: dir, OldText: "a", NewText: "b", ReplaceAll: true})
	if err != nil || ed.ReplacedCount != 3 {
		t.Fatalf("edit all = count %d err %v", ed.ReplacedCount, err)
	}
	if info, err = os.Stat(path); err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("edit did not preserve existing mode: info=%v err=%v", info, err)
	}
}

func TestFileOp_EditSafety(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.txt")
	root := filepath.Dir(path)
	if err := os.WriteFile(path, []byte("a a a"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ambiguous single edit → ErrFileOpAmbiguous with the match count.
	_, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: root, OldText: "a", NewText: "b"})
	var amb *FileOpAmbiguousError
	if !errors.As(err, &amb) || amb.Count != 3 {
		t.Fatalf("ambiguous edit = %v, want FileOpAmbiguousError count 3", err)
	}
	if !errors.Is(err, ErrFileOpAmbiguous) {
		t.Errorf("ambiguous error should match ErrFileOpAmbiguous sentinel")
	}
	if got, _ := os.ReadFile(path); string(got) != "a a a" {
		t.Errorf("file changed on ambiguous edit: %q", got)
	}

	// Stale guard: wrong expected hash → ErrFileOpStale, file unchanged.
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: root, OldText: "a", NewText: "b", ReplaceAll: true, ExpectedSHA256: "deadbeef"})
	if !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("stale edit = %v, want ErrFileOpStale", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a a a" {
		t.Errorf("file changed on stale edit: %q", got)
	}

	// Correct expected hash (from a read) → succeeds.
	rd, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: root, OldText: "a", NewText: "b", ReplaceAll: true, ExpectedSHA256: rd.SHA256}); err != nil {
		t.Fatalf("edit with correct expected hash: %v", err)
	}

	// No-op edit (old==new after replace) → ErrFileOpNoOp.
	if err := os.WriteFile(path, []byte("zzz"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: root, OldText: "z", NewText: "z", ReplaceAll: true})
	if !errors.Is(err, ErrFileOpNoOp) {
		t.Fatalf("no-op edit = %v, want ErrFileOpNoOp", err)
	}
}

func TestFileOp_ReadReportsFullSHA256(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f.txt")
	root := filepath.Dir(path)
	body := []byte("the quick brown fox")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	// Full read and a windowed read both report the FULL-file hash.
	full, _ := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: root})
	win, _ := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Root: root, Offset: 4, Limit: 5})
	if full.SHA256 != want || win.SHA256 != want {
		t.Errorf("sha256 full=%q window=%q, want %q", full.SHA256, win.SHA256, want)
	}
}

func TestFileOp_PreservesModeOnOverwriteAndEdit(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "run.sh")
	root := filepath.Dir(path)

	// New file → 0600.
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("#!/bin/sh\necho a\n")}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o, want 600", info.Mode().Perm())
	}
	// Agent makes it executable.
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// Overwrite must keep 0755 (pre-#784 os.WriteFile behavior).
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("#!/bin/sh\necho b\n")}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Errorf("mode after overwrite = %o, want 755 preserved", info.Mode().Perm())
	}
	// Edit must also keep 0755 (unique old_text).
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, Root: root, OldText: "echo b", NewText: "echo c"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Errorf("mode after edit = %o, want 755 preserved", info.Mode().Perm())
	}
}

func TestFileOp_TypedErrors(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: filepath.Join(dir, "nope"), Root: dir}); !errors.Is(err, ErrFileOpNotFound) {
		t.Errorf("read missing = %v, want ErrFileOpNotFound", err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: subdir, Root: dir}); !errors.Is(err, ErrFileOpIsDirectory) {
		t.Errorf("read dir = %v, want ErrFileOpIsDirectory", err)
	}
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: f, Root: dir, OldText: "absent", NewText: "q"}); !errors.Is(err, ErrFileOpOldTextAbsent) {
		t.Errorf("edit absent = %v, want ErrFileOpOldTextAbsent", err)
	}
	// A failed edit leaves the file byte-identical.
	if got, _ := os.ReadFile(f); string(got) != "xyz" {
		t.Errorf("file changed on failed edit: %q", got)
	}
}

func TestFileOp_RejectsRelativePathAndClosed(t *testing.T) {
	sb := fileopTestSandbox(t)
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "relative/path", Root: "/tmp"}); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path: want absolute-path error, got %v", err)
	}
	sb.Close()
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "/tmp/x", Root: "/tmp"}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed sandbox: want ErrClosed, got %v", err)
	}
}

func TestFileOp_RejectsScopeEscapeAndSymlink(t *testing.T) {
	sb := fileopTestSandbox(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: outside, Root: root,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("scope escape = %v, want ErrFileOpUnsafePath", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: link, Root: root,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("final symlink = %v, want ErrFileOpUnsafePath", err)
	}
}

func TestFileOp_BoundRootIdentityRejectsDirectoryExchange(t *testing.T) {
	sb := fileopTestSandbox(t)
	base := t.TempDir()
	bound := filepath.Join(base, "conv-attacker")
	victim := filepath.Join(base, "conv-victim")
	if err := os.MkdirAll(bound, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bound, "secret.txt"), []byte("attacker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sb.BindFileOpRoot(context.Background(), bound); err != nil {
		t.Fatal(err)
	}
	held := filepath.Join(base, "conv-attacker-held")
	if err := os.Rename(bound, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(victim, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: filepath.Join(bound, "secret.txt"), Root: bound,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("exchanged root read = %v, want ErrFileOpUnsafePath", err)
	}
}

// A sub-agent's file tools scope every request to <bound root>/subagents/<id>
// (#1043) — a root BENEATH the one bound for the turn, not equal to it. Such a
// request must inherit the bound root's capability and identity guard: the
// container backends refuse an unbound writable root outright, and the host
// executor must not treat the sub-tree as an unguarded root either. Exchanging
// the bound directory after binding proves the guard travels with the
// re-anchored request — before the fix the sub-root read returned the victim's
// bytes because rootBound was only ever set on an exact match.
func TestFileOp_SubTreeRootInheritsBoundRootIdentity(t *testing.T) {
	sb := fileopTestSandbox(t)
	base := t.TempDir()
	bound := filepath.Join(base, "conv-parent")
	victim := filepath.Join(base, "conv-victim")
	for _, dir := range []string{bound, victim} {
		if err := os.MkdirAll(filepath.Join(dir, "subagents", "child-1"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	childRoot := filepath.Join(bound, "subagents", "child-1")
	if err := os.WriteFile(filepath.Join(childRoot, "notes.txt"), []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "subagents", "child-1", "notes.txt"), []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sb.BindFileOpRoot(context.Background(), bound); err != nil {
		t.Fatal(err)
	}

	// The happy path: a sub-root read/write works and lands in the sub-tree.
	res, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: filepath.Join(childRoot, "notes.txt"), Root: childRoot,
	})
	if err != nil {
		t.Fatalf("sub-root read: %v", err)
	}
	if string(res.Data) != "parent" {
		t.Fatalf("sub-root read = %q, want %q", res.Data, "parent")
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpWrite, Path: filepath.Join(childRoot, "out.txt"), Root: childRoot, Data: []byte("child wrote"),
	}); err != nil {
		t.Fatalf("sub-root write: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(childRoot, "out.txt")); err != nil || string(got) != "child wrote" {
		t.Fatalf("sub-root write landed as (%q, %v)", got, err)
	}

	// A path that escapes the sub-root is still refused by the scope check even
	// though it is inside the bound root: the narrower scope is honoured.
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: filepath.Join(bound, "sibling.txt"), Root: childRoot,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("escape from sub-root = %v, want ErrFileOpUnsafePath", err)
	}

	// Exchange the bound directory: the identity guard must fire for the
	// sub-root request exactly as it does for a request at the bound root.
	held := filepath.Join(base, "conv-parent-held")
	if err := os.Rename(bound, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(victim, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: filepath.Join(childRoot, "notes.txt"), Root: childRoot,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("exchanged bound root via sub-root read = %v, want ErrFileOpUnsafePath", err)
	}
}

type fileOpOutcome struct {
	result FileOpResult
	err    error
}

func waitForFileOpMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fileop rendezvous marker did not appear: %s", path)
}

// A background process in a persistent sandbox can rename a validated parent
// and replace it with a sibling-conversation symlink between the host check and
// executor use. The helper must keep operating through the already-open parent
// fd, never follow the replacement pathname.
func TestFileOp_SymlinkSwapCannotRedirectReadOrWrite(t *testing.T) {
	sb := fileopTestSandbox(t)
	base := t.TempDir()
	attacker := filepath.Join(base, "conv-attacker")
	victim := filepath.Join(base, "conv-victim")
	if err := os.MkdirAll(filepath.Join(attacker, "read-parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attacker, "read-parent", "secret.txt"), []byte("attacker copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("read", func(t *testing.T) {
		marker := ".fleet-fileop-test-read"
		out := make(chan fileOpOutcome, 1)
		go func() {
			result, err := sb.RunFileOp(context.Background(), FileOpRequest{
				Op: FileOpRead, Path: filepath.Join(attacker, "read-parent", "secret.txt"), Root: attacker,
				testPause: time.Second, testReadyName: marker,
			})
			out <- fileOpOutcome{result: result, err: err}
		}()
		parent := filepath.Join(attacker, "read-parent")
		waitForFileOpMarker(t, filepath.Join(parent, marker))
		held := filepath.Join(attacker, "read-parent-held")
		if err := os.Rename(parent, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, parent); err != nil {
			t.Fatal(err)
		}
		got := <-out
		if got.err != nil || string(got.result.Data) != "attacker copy" {
			t.Fatalf("read followed swapped path: data=%q err=%v", got.result.Data, got.err)
		}
	})

	t.Run("write", func(t *testing.T) {
		parent := filepath.Join(attacker, "write-parent")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := ".fleet-fileop-test-write"
		out := make(chan fileOpOutcome, 1)
		go func() {
			result, err := sb.RunFileOp(context.Background(), FileOpRequest{
				Op: FileOpWrite, Path: filepath.Join(parent, "planted.txt"), Root: attacker, Data: []byte("safe"),
				testPause: time.Second, testReadyName: marker,
			})
			out <- fileOpOutcome{result: result, err: err}
		}()
		waitForFileOpMarker(t, filepath.Join(parent, marker))
		held := filepath.Join(attacker, "write-parent-held")
		if err := os.Rename(parent, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, parent); err != nil {
			t.Fatal(err)
		}
		got := <-out
		if got.err != nil {
			t.Fatalf("write through held dirfd: %v", got.err)
		}
		if _, err := os.Stat(filepath.Join(victim, "planted.txt")); !os.IsNotExist(err) {
			t.Fatalf("write escaped into victim: %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(held, "planted.txt")); err != nil || string(data) != "safe" {
			t.Fatalf("held-directory write = %q err=%v", data, err)
		}
	})
}

// TestContainerFileOpIsolation (podman-gated, e2e-live) proves the file op runs
// INSIDE the container: a write lands in the bind-mounted workspace and is
// visible via an independent RunBash `cat`, and it works with the network
// sealed (lockdown).
func TestContainerFileOpIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tmp := t.TempDir()
	// CI runs rootless and keep-id maps its owner to sandbox uid 1000. Local
	// rootful Podman does not apply that mapping, so make this isolated fixture
	// writable to exercise FileOp rather than host uid topology.
	if err := os.Chmod(tmp, 0o777); err != nil {
		t.Fatal(err)
	}
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused\n"),
		NoNetwork:        true, // lockdown: fileop must not need network
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()
	if err := sb.BindFileOpRoot(ctx, tmp); err != nil {
		t.Fatalf("BindFileOpRoot: %v", err)
	}

	path := tmp + "/report.txt"
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: tmp, Data: []byte("in-container")}); err != nil {
		t.Fatalf("write fileop in lockdown container: %v", err)
	}
	res, err := sb.RunBash(ctx, BashRequest{Command: "cat " + path, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "in-container" {
		t.Fatalf("fileop write not visible in-container: %q", res.Stdout)
	}
}

// TestContainerFileOpCancellationRetiresSandbox proves cancellation cannot
// merely kill the host-side podman client and leave fileops.py to commit later.
// The helper announces that it has opened the destination parent, pauses, and
// would then rename the target. Cancellation must synchronously kill the whole
// container, poison the handle, and keep the delayed target absent.
func TestContainerFileOpCancellationRetiresSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	startCtx, startCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startCancel()
	root := t.TempDir()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	sb, err := NewContainer(startCtx, ContainerConfig{
		Image: testImage(), WorkspaceHostDir: root, BridgeScript: []byte("# unused\n"), NoNetwork: true,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()
	if err := sb.BindFileOpRoot(startCtx, root); err != nil {
		t.Fatalf("BindFileOpRoot: %v", err)
	}

	opCtx, cancel := context.WithCancel(context.Background())
	target := filepath.Join(root, "must-not-land.txt")
	marker := ".fleet-fileop-test-cancel"
	out := make(chan error, 1)
	go func() {
		_, runErr := sb.RunFileOp(opCtx, FileOpRequest{
			Op: FileOpWrite, Path: target, Root: root, Data: []byte("late mutation"),
			testPause: 750 * time.Millisecond, testReadyName: marker,
		})
		out <- runErr
	}()
	waitForFileOpMarker(t, filepath.Join(root, marker))
	cancel()
	if err := <-out; !errors.Is(err, ErrPoisoned) {
		t.Fatalf("cancelled fileop = %v, want ErrPoisoned", err)
	}
	if !sb.Poisoned() {
		t.Fatal("cancelled fileop did not poison the sandbox")
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{
		Op: FileOpRead, Path: target, Root: root,
	}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("poisoned sandbox accepted another op: %v", err)
	}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("poisoned sandbox accepted bash: %v", err)
	}
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "1"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("poisoned sandbox accepted python: %v", err)
	}
	time.Sleep(time.Second) // longer than the helper pause: catches a surviving exec
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("cancelled helper committed after return: stat err=%v", err)
	}
}

func TestContainerFileOpBoundRootRejectsDirectoryExchange(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatal(err)
	}
	bound := filepath.Join(workspace, "conv-attacker")
	victim := filepath.Join(workspace, "conv-victim")
	for _, dir := range []string{bound, victim} {
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bound, "secret.txt"), []byte("attacker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewContainer(ctx, ContainerConfig{
		Image: testImage(), WorkspaceHostDir: workspace, BridgeScript: []byte("# unused\n"), NoNetwork: true,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()
	if err := sb.BindFileOpRoot(ctx, bound); err != nil {
		t.Fatalf("BindFileOpRoot: %v", err)
	}
	held := filepath.Join(workspace, "conv-attacker-held")
	if err := os.Rename(bound, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(victim, bound); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{
		Op: FileOpRead, Path: filepath.Join(bound, "secret.txt"), Root: bound,
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("exchanged in-container root read = %v, want ErrFileOpUnsafePath", err)
	}
}

func TestContainerFileOpReadOnlySupportingMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workspace := t.TempDir()
	docs := t.TempDir()
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(docs, "policy.md")
	if err := os.WriteFile(policy, []byte("governed"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewContainer(ctx, ContainerConfig{
		Image: testImage(), WorkspaceHostDir: workspace, ReadOnlyMounts: []string{docs},
		BridgeScript: []byte("# unused\n"), NoNetwork: true,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()
	if err := sb.BindFileOpRoot(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: policy, Root: docs})
	if err != nil || string(res.Data) != "governed" {
		t.Fatalf("supporting mount read = %q err=%v", res.Data, err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{
		Op: FileOpWrite, Path: policy, Root: docs, Data: []byte("mutated"),
	}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("supporting mount write = %v, want ErrFileOpUnsafePath", err)
	}
}
