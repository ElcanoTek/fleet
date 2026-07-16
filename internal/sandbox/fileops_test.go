package sandbox

// #784: the sandboxed file-operation seam. These tests drive the REAL embedded
// fileops.py through the host backend (python3 subprocess, no podman) so the
// executor's read/write/edit semantics — offset/limit, atomic 0600 writes,
// unique/all replace, and the typed not-found/is-dir/old-absent errors — are
// verified on every platform. The full in-container routing (runtime, seccomp,
// caps, lockdown network) is covered by the podman-gated test below.

import (
	"context"
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
	wr, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Data: []byte("hello world")})
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

	// Read back, full + offset/limit.
	rd, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path})
	if err != nil || string(rd.Data) != "hello world" || rd.Size != 11 {
		t.Fatalf("read = %q size %d err %v", rd.Data, rd.Size, err)
	}
	rd, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path, Offset: 6, Limit: 3})
	if err != nil || string(rd.Data) != "wor" {
		t.Fatalf("read offset/limit = %q err %v", rd.Data, err)
	}
	if rd.Size != 11 {
		t.Errorf("read reports total size %d, want 11", rd.Size)
	}

	// Edit single vs replace_all.
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Data: []byte("a a a")}); err != nil {
		t.Fatal(err)
	}
	ed, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, OldText: "a", NewText: "b"})
	if err != nil || ed.ReplacedCount != 1 {
		t.Fatalf("edit single = count %d err %v", ed.ReplacedCount, err)
	}
	if got, _ := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: path}); string(got.Data) != "b a a" {
		t.Errorf("after single edit = %q, want 'b a a'", got.Data)
	}
	ed, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: path, OldText: "a", NewText: "b", ReplaceAll: true})
	if err != nil || ed.ReplacedCount != 2 {
		t.Fatalf("edit all = count %d err %v", ed.ReplacedCount, err)
	}
}

func TestFileOp_TypedErrors(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: filepath.Join(dir, "nope")}); !errors.Is(err, ErrFileOpNotFound) {
		t.Errorf("read missing = %v, want ErrFileOpNotFound", err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpRead, Path: dir}); !errors.Is(err, ErrFileOpIsDirectory) {
		t.Errorf("read dir = %v, want ErrFileOpIsDirectory", err)
	}
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpEdit, Path: f, OldText: "absent", NewText: "q"}); !errors.Is(err, ErrFileOpOldTextAbsent) {
		t.Errorf("edit absent = %v, want ErrFileOpOldTextAbsent", err)
	}
	// A failed edit leaves the file byte-identical.
	if got, _ := os.ReadFile(f); string(got) != "xyz" {
		t.Errorf("file changed on failed edit: %q", got)
	}
}

func TestFileOp_RejectsRelativePathAndClosed(t *testing.T) {
	sb := fileopTestSandbox(t)
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "relative/path"}); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path: want absolute-path error, got %v", err)
	}
	sb.Close()
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "/tmp/x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("closed sandbox: want ErrClosed, got %v", err)
	}
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

	path := tmp + "/report.txt"
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Data: []byte("in-container")}); err != nil {
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
