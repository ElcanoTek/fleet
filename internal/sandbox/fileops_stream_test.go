package sandbox

// The streamed write behind download_url: chunks append to a hidden partial
// file that is renamed into place without replacing anything, through the
// REAL embedded fileops.py on the host backend (as fileops_test.go does).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func streamPartial(t *testing.T, dir string) string {
	t.Helper()
	name, err := StreamPartialName()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func TestFileOp_StreamAppendsAndCommits(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	partial := streamPartial(t, filepath.Join(root, "sub"))

	chunks := [][]byte{[]byte("alpha,"), []byte("beta,"), []byte("gamma")}
	var offset int64
	for i, c := range chunks {
		req := FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: offset, Data: c}
		if i == len(chunks)-1 {
			req.Final = "report.csv"
		}
		res, err := sb.RunFileOp(ctx, req)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		offset += int64(len(c))
		if res.Size != offset {
			t.Fatalf("chunk %d size = %d, want %d", i, res.Size, offset)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "sub", "report.csv"))
	if err != nil || string(got) != "alpha,beta,gamma" {
		t.Fatalf("committed file %q %v", got, err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
}

func TestFileOp_StreamRefusesWrongOffsetsAndExistingFinals(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	partial := streamPartial(t, root)
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	// Offset 0 again: the partial file must not be re-created.
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Data: []byte("x")}); !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("re-create: %v", err)
	}
	// A gap or overlap is refused.
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 2, Data: []byte("x")}); !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("wrong offset: %v", err)
	}
	// The commit never replaces an existing file.
	existing := filepath.Join(root, "taken.csv")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 3, Final: "taken.csv"}); !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("commit over an existing file: %v", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Fatalf("existing file replaced: %q", got)
	}
	// Abort removes the partial file.
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Abort: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("abort left the partial file: %v", err)
	}
}

func TestFileOp_StreamOnlyTouchesPartialFiles(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	agentFile := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(agentFile, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, req := range []FileOpRequest{
		{Op: FileOpStream, Path: agentFile, Root: root, Offset: 4, Data: []byte("!")},
		{Op: FileOpStream, Path: agentFile, Root: root, Abort: true},
		{Op: FileOpStream, Path: streamPartial(t, root), Root: root, Final: "../escape.txt"},
		{Op: FileOpStream, Path: streamPartial(t, root), Root: root, Final: ".fleet-download-x.partial"},
	} {
		if _, err := sb.RunFileOp(ctx, req); !errors.Is(err, ErrFileOpUnsafePath) {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if got, _ := os.ReadFile(agentFile); !bytes.Equal(got, []byte("mine")) {
		t.Fatalf("agent file touched: %q", got)
	}
}

func TestFileOp_StreamRefusesASymlinkedPartial(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(target, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := streamPartial(t, root)
	if err := os.Symlink(target, partial); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 1, Data: []byte("x")}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Fatalf("append through a symlink: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "v" {
		t.Fatalf("symlink target written: %q", got)
	}
}

// The executor enforces the partial-name rule itself: the Go-side check is
// input validation, and the executor is what runs inside the sandbox.
func TestFileOpsPy_StreamRefusesNonPartialNames(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	root := t.TempDir()
	agentFile := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(agentFile, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, req := range []map[string]any{
		{"op": "stream", "path": agentFile, "root": root, "anchor": root, "offset": 4, "data_b64": "IQ=="},
		{"op": "stream", "path": agentFile, "root": root, "anchor": root, "abort": true},
	} {
		in, _ := json.Marshal(req)
		cmd := exec.Command("python3", "-I", "-c", string(fileOpsScript))
		cmd.Stdin = bytes.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var resp struct {
			OK      bool   `json:"ok"`
			ErrKind string `json:"err_kind"`
		}
		if err := json.Unmarshal(out, &resp); err != nil || resp.OK || resp.ErrKind != "unsafe_path" {
			t.Fatalf("%v: %s", req, out)
		}
	}
	if got, _ := os.ReadFile(agentFile); string(got) != "mine" {
		t.Fatalf("agent file touched: %q", got)
	}
}

func TestFileOp_StreamAbortCreatesNothing(t *testing.T) {
	sb := fileopTestSandbox(t)
	root := t.TempDir()
	missing := filepath.Join(root, "never", "made")
	// Best effort either way: the point is what it leaves on disk.
	_, _ = sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpStream, Path: streamPartial(t, missing), Root: root, Abort: true})
	if _, err := os.Stat(filepath.Join(root, "never")); !os.IsNotExist(err) {
		t.Fatalf("abort created directories: %v", err)
	}
}

func TestFileOp_StreamSweepsStalePartials(t *testing.T) {
	sb := fileopTestSandbox(t)
	root := t.TempDir()
	stale := streamPartial(t, root)
	fresh := streamPartial(t, root)
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("left over"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpStream, Path: streamPartial(t, root), Root: root, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale partial kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent partial (a running download) was removed: %v", err)
	}
}

// On a filesystem without hard links (some RWX volumes) the commit falls back
// to renameat2(RENAME_NOREPLACE), and with neither the commit fails, so an
// existing file is never replaced.
func TestFileOpsPy_StreamCommitsWithoutHardLinks(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	root := t.TempDir()
	noLink := "import errno, os\n" +
		"def _nolink(*a, **k):\n    raise OSError(errno.EPERM, 'hard links not supported')\n" +
		"os.link = _nolink\n"
	prefix := noLink
	run := func(req map[string]any) map[string]any {
		t.Helper()
		in, _ := json.Marshal(req)
		script := prefix + string(fileOpsScript)
		cmd := exec.Command("python3", "-I", "-c", script)
		cmd.Stdin = bytes.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return resp
	}
	name, _ := StreamPartialName()
	partial := filepath.Join(root, name)
	resp := run(map[string]any{"op": "stream", "path": partial, "root": root, "anchor": root, "offset": 0, "data_b64": "YWJj", "final": "r.csv"})
	if resp["ok"] != true {
		t.Fatalf("commit without hard links: %v", resp)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "r.csv")); string(got) != "abc" {
		t.Fatalf("committed %q", got)
	}
	name2, _ := StreamPartialName()
	resp = run(map[string]any{"op": "stream", "path": filepath.Join(root, name2), "root": root, "anchor": root, "offset": 0, "data_b64": "eA==", "final": "r.csv"})
	if resp["ok"] == true || resp["err_kind"] != "stale" {
		t.Fatalf("rename fallback replaced an existing file: %v", resp)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "r.csv")); string(got) != "abc" {
		t.Fatalf("existing file replaced: %q", got)
	}

	// Neither hard links nor renameat2: the commit fails instead of risking a
	// replacing rename.
	prefix = noLink + "import ctypes\nctypes.CDLL = lambda *a, **k: object()\n"
	name3, _ := StreamPartialName()
	resp = run(map[string]any{"op": "stream", "path": filepath.Join(root, name3), "root": root, "anchor": root, "offset": 0, "data_b64": "eQ==", "final": "new.csv"})
	if resp["ok"] == true || !strings.Contains(fmt.Sprint(resp["err"]), "no-replace") {
		t.Fatalf("commit without any no-replace primitive: %v", resp)
	}
	if _, err := os.Stat(filepath.Join(root, "new.csv")); !os.IsNotExist(err) {
		t.Fatalf("file committed without a no-replace primitive: %v", err)
	}
}

func TestFileOp_StreamRefusesASameLengthOverwrite(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	partial := streamPartial(t, root)
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	// Another process rewrites the staged bytes in place: same inode, same size.
	f, err := os.OpenFile(partial, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 3, Data: []byte("def"),
		Final: "r.csv", ExpectedSHA256: sha256Hex([]byte("abcdef"))})
	if !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("committed overwritten bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "r.csv")); !os.IsNotExist(err) {
		t.Fatalf("destination created: %v", err)
	}
}

func TestFileOp_StreamCTimeChainCatchesChangesBetweenSteps(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()

	// Untouched between steps: the chain holds and the commit lands.
	partial := streamPartial(t, root)
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Data: []byte("abc")})
	if err != nil || res.CTimeNS == 0 {
		t.Fatalf("first chunk: ctime %d, %v", res.CTimeNS, err)
	}
	res, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 3, Data: []byte("def"), ExpectedCTimeNS: res.CTimeNS})
	if err != nil {
		t.Fatalf("chained chunk: %v", err)
	}
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 6, Final: "ok.csv", CommitOnly: true, ExpectedCTimeNS: res.CTimeNS}); err != nil {
		t.Fatalf("chained commit: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "ok.csv")); err != nil || string(got) != "abcdef" {
		t.Fatalf("committed %q %v", got, err)
	}

	// Rewritten in place between steps (same inode, same size): both the next
	// append and the commit are refused.
	overwrite := func(path string) {
		t.Helper()
		time.Sleep(20 * time.Millisecond) // past a coarse timestamp tick
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	partial = streamPartial(t, root)
	res, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Data: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	overwrite(partial)
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 3, Data: []byte("def"), ExpectedCTimeNS: res.CTimeNS})
	if !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("appended to a changed partial: %v", err)
	}
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpStream, Path: partial, Root: root, Offset: 3, Final: "bad.csv", CommitOnly: true, ExpectedCTimeNS: res.CTimeNS})
	if !errors.Is(err, ErrFileOpStale) {
		t.Fatalf("committed a changed partial: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bad.csv")); !os.IsNotExist(err) {
		t.Fatalf("destination created: %v", err)
	}
}

func TestFileOpRequest_TimeoutIsBounded(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{
		{0, fileOpTimeout},
		{time.Second, fileOpTimeout},
		{3 * time.Minute, 3 * time.Minute},
		{time.Hour, maxFileOpTimeout},
	} {
		if got := (FileOpRequest{Timeout: tc.in}).timeout(); got != tc.want {
			t.Errorf("timeout(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
