package sandbox

// #1745: write_file's overwrite report and stale-content guard, through the
// REAL embedded fileops.py on the host backend (as fileops_test.go does).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// unchanged fails the test unless path still holds want with its old mtime.
func unchanged(t *testing.T, path string, want []byte, mtime time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || !info.ModTime().Equal(mtime) {
		t.Fatalf("file changed: %q (mtime %v, was %v), want %q untouched", got, info.ModTime(), mtime, want)
	}
}

func TestFileOp_WriteReportsCreateAndOverwrite(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")

	created, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("one\ntwo\n")})
	if err != nil {
		t.Fatal(err)
	}
	if created.Replaced || created.OldSHA256 != "" || created.Diff != "" {
		t.Fatalf("a new file reported as an overwrite: %+v", created)
	}

	old := []byte("one\ntwo\n")
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("one\nthree\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced || res.OldSHA256 != sha256Hex(old) || res.OldSize != int64(len(old)) {
		t.Fatalf("overwrite = replaced %v old_sha256 %q old_size %d; want true %q %d", res.Replaced, res.OldSHA256, res.OldSize, sha256Hex(old), len(old))
	}
	if res.SHA256 != sha256Hex([]byte("one\nthree\n")) {
		t.Errorf("sha256 = %q, want the new content's", res.SHA256)
	}
	if !strings.Contains(res.Diff, "-two\n") || !strings.Contains(res.Diff, "+three\n") || res.Added != 1 || res.Removed != 1 {
		t.Errorf("diff = %q (+%d/-%d), want -two/+three (+1/-1)", res.Diff, res.Added, res.Removed)
	}
	if res.DiffNote != "" {
		t.Errorf("diff note = %q, want none for a text diff", res.DiffNote)
	}

	same, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("one\nthree\n")})
	if err != nil {
		t.Fatal(err)
	}
	if !same.Replaced || same.OldSHA256 != same.SHA256 || same.Diff != "" {
		t.Errorf("identical rewrite = %+v, want replaced with old == new hash and no diff", same)
	}
}

// Binary content and files over the diff cap still report their hashes, with
// a note instead of a diff; a long diff is cut and says the write is whole.
func TestFileOp_WriteDiffLimits(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "f")

	cases := []struct {
		name     string
		old, new []byte
		note     string
	}{
		{"binary old", []byte{0xff, 0xfe, 0x00, 'a'}, []byte("text\n"), "binary"},
		{"binary new", []byte("text\n"), []byte{0xff, 0x00}, "binary"},
		{"old over the cap", bytes.Repeat([]byte("x\n"), 256*1024/2+1), []byte("small\n"), "over 256 KB"},
		{"new over the cap", []byte("small\n"), bytes.Repeat([]byte("y\n"), 256*1024/2+1), "over 256 KB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.old, 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: tc.new})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Replaced || res.OldSHA256 != sha256Hex(tc.old) || res.OldSize != int64(len(tc.old)) {
				t.Errorf("replaced %v old_sha256 %q old_size %d, want true %q %d", res.Replaced, res.OldSHA256, res.OldSize, sha256Hex(tc.old), len(tc.old))
			}
			if res.Diff != "" || !strings.Contains(res.DiffNote, tc.note) {
				t.Errorf("diff %q note %q, want no diff and a note naming %q", res.Diff, res.DiffNote, tc.note)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, tc.new) {
				t.Errorf("file not written")
			}
		})
	}

	t.Run("long diff", func(t *testing.T) {
		var before, after bytes.Buffer
		for i := range 2000 {
			before.WriteString("old line " + strings.Repeat("a", i%7) + "\n")
			after.WriteString("new line " + strings.Repeat("b", i%5) + "\n")
		}
		if err := os.WriteFile(path, before.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: after.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Diff) > 4096+100 || !strings.Contains(res.Diff, "diff truncated (write applied in full)") {
			t.Errorf("diff of %d bytes, ending %q; want it cut near 4 KB with the write note", len(res.Diff), res.Diff[max(0, len(res.Diff)-80):])
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, after.Bytes()) {
			t.Errorf("file not written in full")
		}
	})
}

func TestFileOp_WriteExpectedHash(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "report.md")
	old := []byte("draft\n")
	if err := os.WriteFile(path, old, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	time.Sleep(10 * time.Millisecond) // so a rewrite would move the mtime

	write := func(expected string, path string) (FileOpResult, error) {
		return sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("final\n"), ExpectedSHA256: expected})
	}

	t.Run("mismatch", func(t *testing.T) {
		_, err := write(sha256Hex([]byte("something else")), path)
		var stale *FileOpStaleError
		if !errors.Is(err, ErrFileOpStale) || !errors.As(err, &stale) {
			t.Fatalf("err = %v, want a *FileOpStaleError", err)
		}
		if !strings.Contains(err.Error(), "current "+sha256Hex(old)) || strings.Count(err.Error(), "changed since it was last read") != 1 {
			t.Errorf("message = %q, want the current hash, said once", err.Error())
		}
		unchanged(t, path, old, mtime)
	})

	t.Run("absent but the file exists", func(t *testing.T) {
		_, err := write(ExpectAbsent, path)
		if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "already exists (sha256 "+sha256Hex(old)) {
			t.Fatalf("err = %v, want stale naming the existing file's hash", err)
		}
		unchanged(t, path, old, mtime)
	})

	t.Run("hash for a file that is gone", func(t *testing.T) {
		gone := filepath.Join(root, "gone.md")
		_, err := write(sha256Hex(old), gone)
		if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("err = %v, want stale: the file does not exist", err)
		}
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("a guarded write created the missing file: %v", err)
		}
	})

	t.Run("hash in a directory that is gone", func(t *testing.T) {
		dir := filepath.Join(root, "missing-dir")
		_, err := write(sha256Hex(old), filepath.Join(dir, "x.md"))
		if !errors.Is(err, ErrFileOpStale) {
			t.Fatalf("err = %v, want stale", err)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("a guarded write created the missing directory: %v", err)
		}
	})

	t.Run("absent creates, directories too", func(t *testing.T) {
		fresh := filepath.Join(root, "new-dir", "fresh.md")
		res, err := write(ExpectAbsent, fresh)
		if err != nil || res.Replaced {
			t.Fatalf("create-only write = %+v, %v; want a created file", res, err)
		}
		if got, _ := os.ReadFile(fresh); string(got) != "final\n" {
			t.Errorf("created %q", got)
		}
	})

	t.Run("matching hash, any case and prefix", func(t *testing.T) {
		res, err := write("  SHA256:"+strings.ToUpper(sha256Hex(old))+" ", path)
		if err != nil {
			t.Fatalf("guarded write with the current hash: %v", err)
		}
		if !res.Replaced || res.OldSHA256 != sha256Hex(old) {
			t.Errorf("result = %+v, want an overwrite of the old content", res)
		}
		if got, _ := os.ReadFile(path); string(got) != "final\n" {
			t.Errorf("file = %q, want the new content", got)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
			t.Errorf("mode = %o, want the old file's 0640 kept", info.Mode().Perm())
		}
	})
}

// A FIFO (or other non-regular entry) at the path is replaced without being
// opened for a blocking read, and is never mistaken for a hash match.
func TestFileOp_WriteOverNonRegularFile(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	for _, expected := range []string{sha256Hex(nil), ExpectAbsent} {
		if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: fifo, Root: root, Data: []byte("x"), ExpectedSHA256: expected}); !errors.Is(err, ErrFileOpStale) {
			t.Errorf("expected %q over a FIFO = %v, want ErrFileOpStale", expected, err)
		}
		if info, err := os.Lstat(fifo); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("FIFO replaced by a refused write: %v %v", info, err)
		}
	}
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: fifo, Root: root, Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Replaced || res.OldSHA256 != "" || !strings.Contains(res.DiffNote, "not a regular file") {
		t.Errorf("result = %+v, want a replaced non-regular entry with no old hash", res)
	}
	if got, _ := os.ReadFile(fifo); string(got) != "x" {
		t.Errorf("file = %q, want x", got)
	}
}

// A file that appears between the executor seeing none and committing must
// not be replaced and then reported as created. The race is forced by making
// the executor's first look find nothing.
func TestFileOp_WriteRefusesAFileThatAppeared(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fileop seam")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "fileops.py"), fileOpsScript, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "late.txt")
	if err := os.WriteFile(path, []byte("arrived first"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"", ExpectAbsent} {
		req, err := json.Marshal(map[string]any{
			"op": "write", "path": path, "root": root, "anchor": root,
			"data_b64": "bmV3", "expected_sha256": expected,
		})
		if err != nil {
			t.Fatal(err)
		}
		driver := `import json, sys
sys.path.insert(0, sys.argv[1])
import fileops
real, seen = fileops._lstat_at, []
def first_look_finds_nothing(parent_fd, name):
    seen.append(name)
    return None if len(seen) == 1 else real(parent_fd, name)
fileops._lstat_at = first_look_finds_nothing
print(json.dumps(fileops.do_write(json.loads(sys.argv[2]))))
`
		out, err := exec.Command("python3", "-I", "-c", driver, mod, string(req)).CombinedOutput()
		if err != nil {
			t.Fatalf("driver: %v\n%s", err, out)
		}
		_, err = decodeFileOpResponse(out)
		if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "appeared") {
			t.Errorf("expected %q: err = %v, want stale: a file appeared", expected, err)
		}
		if got, _ := os.ReadFile(path); string(got) != "arrived first" {
			t.Fatalf("the file that appeared was replaced: %q", got)
		}
	}
}
