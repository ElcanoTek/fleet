package sandbox

// #1745: write_file's overwrite report and stale-content guard, and the
// bounded diff it shares with edit_file, through the REAL embedded fileops.py
// on the host backend (as fileops_test.go does).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// runPatchedWrite is runPatchedOp for a write.
func runPatchedWrite(t *testing.T, req map[string]any, patch string) (FileOpResult, error) {
	t.Helper()
	return runPatchedOp(t, "write", req, patch)
}

// runPatchedOp runs fileops.do_<op>(req) under python3 after applying patch,
// Python run once `fileops` (the embedded script) is imported, and decodes
// the result. It forces races and failures that real timing cannot produce
// on demand. A helper that hangs fails the test after 15 s.
func runPatchedOp(t *testing.T, op string, req map[string]any, patch string) (FileOpResult, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fileop seam")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "fileops.py"), fileOpsScript, 0o600); err != nil {
		t.Fatal(err)
	}
	req["op"] = op
	if _, ok := req["anchor"]; !ok {
		req["anchor"] = req["root"]
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	driver := "import json, os, sys\nsys.path.insert(0, sys.argv[1])\nimport fileops\n" + patch +
		"\nprint(json.dumps(fileops.do_" + op + "(json.loads(sys.argv[2]))))\n"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "python3", "-I", "-c", driver, mod, string(raw)).Output()
	if ctx.Err() != nil {
		t.Fatalf("fileops.py hung (killed after 15 s)")
	}
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	return decodeFileOpResponse(out)
}

// afterLook wraps fileops._lstat_at so that, once it has been called n times,
// it runs action (Python using parent_fd and name) and then answers.
// nothingFor makes the first m calls report no file.
func afterLook(n int, action string) string {
	return fmt.Sprintf(`real_lstat, looks = fileops._lstat_at, []
def wrapped(parent_fd, name):
    looks.append(name)
    info = real_lstat(parent_fd, name)
    if len(looks) == %d:
%s
    return info
fileops._lstat_at = wrapped
`, n, indent(action, "        "))
}

func indent(code, prefix string) string {
	lines := strings.Split(strings.TrimRight(code, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// A file that appears while a write that saw none is in progress must not
// be replaced and reported as created: whether it appears right after the
// executor looks, or while the new content is being written to its temp file.
func TestFileOp_WriteRefusesAFileThatAppeared(t *testing.T) {
	for _, tc := range []struct {
		name  string
		blind int // how many looks report no file
	}{
		{"before the write starts", 1},
		{"before the rename", 2},
	} {
		for _, expected := range []string{"", ExpectAbsent} {
			t.Run(tc.name+"/"+expected, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "late.txt")
				if err := os.WriteFile(path, []byte("arrived first"), 0o600); err != nil {
					t.Fatal(err)
				}
				patch := fmt.Sprintf(`real_lstat, looks = fileops._lstat_at, []
def blind(parent_fd, name):
    looks.append(name)
    return None if len(looks) <= %d else real_lstat(parent_fd, name)
fileops._lstat_at = blind
`, tc.blind)
				_, err := runPatchedWrite(t, map[string]any{"path": path, "root": root, "data_b64": "bmV3", "expected_sha256": expected}, patch)
				if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "appeared") {
					t.Errorf("err = %v, want stale: a file appeared", err)
				}
				if got, _ := os.ReadFile(path); string(got) != "arrived first" {
					t.Fatalf("the file that appeared was replaced: %q", got)
				}
			})
		}
	}
}

// The final entry swapped after the executor's look, before it reads the old
// file. A symlink is refused as unsafe (the no-follow open); any other swap is
// told apart from the file lstat saw (identity and type checks) and is never
// described as the replaced file. A plain write then still goes ahead, as it
// did before #1745, without an old hash.
func TestFileOp_WriteFinalEntrySwappedBeforeTheRead(t *testing.T) {
	orig := []byte("original\n")
	cases := []struct {
		name, expected, swap string
		wantErr              error
	}{
		{"to a symlink, guarded", sha256Hex(orig), `os.rename(name, name + ".held", src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
os.symlink(OUTSIDE, name, dir_fd=parent_fd)`, ErrFileOpUnsafePath},
		{"to another file", "", `fd = os.open("other", os.O_WRONLY | os.O_CREAT, 0o600, dir_fd=parent_fd)
os.write(fd, b"other file\n")
os.close(fd)
os.rename("other", name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)`, nil},
		{"to a FIFO", "", `os.unlink(name, dir_fd=parent_fd)
os.mkfifo(name, dir_fd=parent_fd)`, nil},
		{"deleted", "", `os.unlink(name, dir_fd=parent_fd)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside.txt")
			if err := os.WriteFile(outside, []byte("outside secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "f.txt")
			if err := os.WriteFile(path, orig, 0o600); err != nil {
				t.Fatal(err)
			}
			patch := "OUTSIDE = " + strconvQuote(outside) + "\n" + afterLook(1, tc.swap)
			res, err := runPatchedWrite(t, map[string]any{"path": path, "root": root, "data_b64": "bmV3Cg==", "expected_sha256": tc.expected}, patch)
			if got, _ := os.ReadFile(outside); string(got) != "outside secret\n" {
				t.Fatalf("the file outside the root changed: %q", got)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("a plain write over a swapped entry was refused: %v", err)
			}
			if !res.Replaced || res.OldSHA256 != "" || !strings.Contains(res.DiffNote, "could not be read") || strings.Contains(res.DiffNote, "retry") {
				t.Errorf("result = %+v, want a replaced file with no old hash, noted as unread (and no advice to retry a write that went through)", res)
			}
			if got, _ := os.ReadFile(path); string(got) != "new\n" {
				t.Errorf("file = %q, want the new content", got)
			}
		})
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A guarded write hashes the file again just before the rename: a change made
// after its first check is refused as stale, naming the current hash, and the
// change survives. So is a FIFO swapped in, which the re-check opens
// non-blocking and refuses rather than waiting on.
func TestFileOp_GuardedWriteRechecksBeforeTheRename(t *testing.T) {
	for _, tc := range []struct {
		name, change, want string
		check              func(t *testing.T, path string)
	}{
		{"content changed", `fd = os.open(name, os.O_WRONLY | os.O_TRUNC, dir_fd=parent_fd)
os.write(fd, b"concurrent\n")
os.close(fd)`, "current " + sha256Hex([]byte("concurrent\n")), func(t *testing.T, path string) {
			if got, _ := os.ReadFile(path); string(got) != "concurrent\n" {
				t.Fatalf("the concurrent change was overwritten: %q", got)
			}
		}},
		{"FIFO swapped in", `os.unlink(name, dir_fd=parent_fd)
os.mkfifo(name, dir_fd=parent_fd)`, "current unknown", func(t *testing.T, path string) {
			if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("the FIFO was replaced: %v %v", info, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "f.txt")
			orig := []byte("original\n")
			if err := os.WriteFile(path, orig, 0o600); err != nil {
				t.Fatal(err)
			}
			patch := `real_read = fileops._read_replaced
def read_then_change(parent_fd, name, seen):
    result = real_read(parent_fd, name, seen)
` + indent(tc.change, "    ") + `
    return result
fileops._read_replaced = read_then_change
`
			_, err := runPatchedWrite(t, map[string]any{"path": path, "root": root, "data_b64": "bmV3Cg==", "expected_sha256": sha256Hex(orig)}, patch)
			if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want stale naming %q", err, tc.want)
			}
			tc.check(t, path)
		})
	}
}

// A file that grows while it is hashed has no trustworthy hash: a plain write
// goes ahead without one, a guarded write is refused.
func TestFileOp_WriteHashOfAGrowingFile(t *testing.T) {
	for _, expected := range []string{"", "guard"} {
		t.Run("expected="+expected, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "log.txt")
			orig := []byte("line 1\n")
			if err := os.WriteFile(path, orig, 0o600); err != nil {
				t.Fatal(err)
			}
			if expected == "guard" {
				expected = sha256Hex(orig)
			}
			patch := "PATH = " + strconvQuote(path) + `
real_os_read, grown = os.read, []
def read_while_growing(fd, n):
    data = real_os_read(fd, n)
    if not grown:
        grown.append(1)
        with open(PATH, "ab") as f:
            f.write(b"line 2\n")
    return data
os.read = read_while_growing
`
			res, err := runPatchedWrite(t, map[string]any{"path": path, "root": root, "data_b64": "bmV3Cg==", "expected_sha256": expected}, patch)
			if expected != "" {
				if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "changed while its hash was computed") {
					t.Fatalf("guarded write over a growing file = %v, want stale: it changed while hashed", err)
				}
				return
			}
			if err != nil || !res.Replaced || res.OldSHA256 != "" || !strings.Contains(res.DiffNote, "could not be read") {
				t.Fatalf("plain write over a growing file = %+v, %v; want it written with no old hash", res, err)
			}
		})
	}
}

// A file the executor cannot read (mode 0200) is still replaced by a plain
// write, as before #1745, with no old hash; a guard cannot be checked, so a
// guarded write is refused.
func TestFileOp_WriteOverAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0200 file")
	}
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "wo.txt")
	if err := os.WriteFile(path, []byte("secret\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	for expected, want := range map[string]string{
		sha256Hex([]byte("secret\n")): "Permission denied",
		ExpectAbsent:                  "already exists (7 bytes; the old file could not be read (Permission denied))",
	} {
		if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("x"), ExpectedSHA256: expected}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q over an unreadable file = %v, want a refusal saying %q", expected, err, want)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "secret\n" {
		t.Fatalf("a refused write changed the file: %q", got)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("new\n")})
	if err != nil {
		t.Fatalf("plain write over an unreadable file: %v", err)
	}
	if !res.Replaced || res.OldSHA256 != "" || res.OldSize != 7 || !strings.Contains(res.DiffNote, "could not be read (Permission denied)") {
		t.Errorf("result = %+v, want a replaced 7-byte file with no hash, noted as unreadable", res)
	}
}

// A plain write over a file larger than the report cap does not read it: the
// hash is only a report, and a read that outlived the file-op timeout would
// retire the sandbox. A hash guard still reads and checks it. The big file is
// sparse, so the test writes no real 64 MB.
func TestFileOp_WriteSkipsHashingAHugeReplacedFile(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "dump.bin")
	const size = 64*1024*1024 + 1
	mkBig := func() {
		t.Helper()
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
	}

	mkBig()
	res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("small\n")})
	if err != nil {
		t.Fatalf("plain write over a huge file: %v", err)
	}
	if !res.Replaced || res.OldSHA256 != "" || res.OldSize != size || !strings.Contains(res.DiffNote, "over 64 MB") {
		t.Errorf("result = %+v, want a replaced %d-byte file with no hash, noted as over 64 MB", res, size)
	}
	if got, _ := os.ReadFile(path); string(got) != "small\n" {
		t.Fatalf("file not written: %d bytes", len(got))
	}

	mkBig()
	if _, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("x"), ExpectedSHA256: ExpectAbsent}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("absent over a huge file = %v, want a refusal", err)
	}
	zeros := sha256Hex(make([]byte, size))
	_, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("x"), ExpectedSHA256: sha256Hex([]byte("other"))})
	if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), zeros) {
		t.Errorf("mismatched guard over a huge file = %v, want stale naming the current hash", err)
	}
	res, err = sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte("guarded\n"), ExpectedSHA256: zeros})
	if err != nil || res.OldSHA256 != zeros {
		t.Errorf("matching guard over a huge file = %+v, %v; want written with old_sha256 %s", res, err, zeros)
	}
}

// view_file and edit_file refuse a FIFO at once rather than blocking on it
// until the file-op timeout retires the sandbox.
func TestFileOp_ReadAndEditRefuseAFIFO(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	for _, req := range []FileOpRequest{
		{Op: FileOpRead, Path: fifo, Root: root},
		{Op: FileOpEdit, Path: fifo, Root: root, OldText: "a", NewText: "b"},
	} {
		if _, err := sb.RunFileOp(ctx, req); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("%s on a FIFO = %v, want a not-a-regular-file error", req.Op, err)
		}
	}
}

// A plain write whose workspace root is gone fails as a write error, not as a
// stale hash it never had.
func TestFileOp_PlainWriteWithAMissingRootIsNotStale(t *testing.T) {
	sb := fileopTestSandbox(t)
	root := filepath.Join(t.TempDir(), "gone")
	_, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpWrite, Path: filepath.Join(root, "f"), Root: root, Data: []byte("x")})
	if err == nil || errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "write failed") {
		t.Errorf("err = %v, want a write failure that is not stale", err)
	}
}

// Every backend sends a write's expected hash: they share one encoder.
func TestEncodeFileOpWireCarriesTheWriteGuard(t *testing.T) {
	for _, tc := range []struct {
		req  FileOpRequest
		want any
	}{
		{FileOpRequest{Op: FileOpWrite, Path: "/w/f", Root: "/w", Data: []byte("x"), ExpectedSHA256: "abc"}, "abc"},
		{FileOpRequest{Op: FileOpWrite, Path: "/w/f", Root: "/w", Data: []byte("x"), ExpectedSHA256: ExpectAbsent}, ExpectAbsent},
		{FileOpRequest{Op: FileOpWrite, Path: "/w/f", Root: "/w", Data: []byte("x")}, nil},
		{FileOpRequest{Op: FileOpEdit, Path: "/w/f", Root: "/w", OldText: "a", NewText: "b", ExpectedSHA256: "abc"}, "abc"},
	} {
		raw, err := encodeFileOpWire(tc.req, "/w")
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if got := wire["expected_sha256"]; got != tc.want {
			t.Errorf("%s with %q: expected_sha256 = %v, want %v", tc.req.Op, tc.req.ExpectedSHA256, got, tc.want)
		}
	}
}

// The diff of a write covers only the changed window, with the file's own
// line numbers, so a small change in a long file is cheap; a change spread
// over more lines than the cap gets a note instead of a slow diff.
func TestFileOp_WriteDiffWindow(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "big.txt")
	// 40,000 six-byte lines: 240,000 bytes, under the 256 KB size cap, so
	// only the line cap can stop a diff.
	lines := make([]string, 40000)
	for i := range lines {
		lines[i] = fmt.Sprintf("%05d\n", i+1)
	}
	old := strings.Join(lines, "")
	write := func(t *testing.T, before, after string) FileOpResult {
		t.Helper()
		if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res, err := sb.RunFileOp(ctx, FileOpRequest{Op: FileOpWrite, Path: path, Root: root, Data: []byte(after)})
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("write took %v", d)
		}
		return res
	}

	t.Run("one line changed deep in a long file", func(t *testing.T) {
		changed := strings.Replace(old, "30000\n", "thirty\n", 1)
		res := write(t, old, changed)
		if !strings.Contains(res.Diff, "@@ -29997,7 +29997,7 @@\n") || !strings.Contains(res.Diff, "-30000\n+thirty\n") || res.Added != 1 || res.Removed != 1 {
			t.Errorf("diff = %q (+%d/-%d), want one changed line at 30000 with file line numbers", res.Diff, res.Added, res.Removed)
		}
	})

	t.Run("every other line changed", func(t *testing.T) {
		alt := make([]string, len(lines))
		for i, l := range lines {
			if i%2 == 1 {
				l = "x" + l[1:] // same length, so the file stays under the size cap
			}
			alt[i] = l
		}
		res := write(t, old, strings.Join(alt, ""))
		if res.Diff != "" || !strings.Contains(res.DiffNote, "spans over 2000 lines") || res.OldSHA256 != sha256Hex([]byte(old)) {
			t.Errorf("result = diff %d bytes, note %q, old hash %q; want no diff, the span note and the old hash", len(res.Diff), res.DiffNote, res.OldSHA256)
		}
	})

	t.Run("a file of exactly the size cap is diffed", func(t *testing.T) {
		line := strings.Repeat("a", 1023) + "\n"
		exact := strings.Repeat(line, 256)
		res := write(t, exact, "first\n"+exact[len(line):])
		if res.DiffNote != "" || !strings.Contains(res.Diff, "+first\n") {
			t.Errorf("note %q, diff %d bytes; want a diff of a 256 KB file", res.DiffNote, len(res.Diff))
		}
	})
}

// runEdit writes content to a fresh file and edits it through the host
// sandbox's fileops.py, failing the test if the edit took over 10 s or was
// not applied in full.
func runEdit(t *testing.T, content string, req FileOpRequest) FileOpResult {
	t.Helper()
	sb := fileopTestSandbox(t)
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	req.Op, req.Path, req.Root = FileOpEdit, path, root
	start := time.Now()
	res, err := sb.RunFileOp(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("edit took %v", d)
	}
	n := 1
	if req.ReplaceAll {
		n = -1
	}
	if got, _ := os.ReadFile(path); string(got) != strings.Replace(content, req.OldText, req.NewText, n) {
		t.Error("the edit was not applied in full")
	}
	return res
}

// numberedLines is 40,000 lines, "00001\n" to "40000\n".
func numberedLines() []string {
	lines := make([]string, 40000)
	for i := range lines {
		lines[i] = fmt.Sprintf("%05d\n", i+1)
	}
	return lines
}

// edit_file's diff covers only the lines around its replacements: a
// replace_all through a long file used to diff the whole file, after the edit
// had landed, which took longer than the file-op timeout, and decoding the
// whole file ran large ones out of sandbox memory.
func TestFileOp_EditDiffWindow(t *testing.T) {
	lines := numberedLines()

	t.Run("one line changed deep in a long file", func(t *testing.T) {
		res := runEdit(t, strings.Join(lines, ""), FileOpRequest{OldText: "30000\n", NewText: "thirty\n"})
		if !strings.Contains(res.Diff, "@@ -29997,7 +29997,7 @@\n") || !strings.Contains(res.Diff, "-30000\n+thirty\n") || res.Added != 1 || res.Removed != 1 || res.DiffNote != "" {
			t.Errorf("diff = %q (+%d/-%d, note %q), want one changed line at 30000 with file line numbers", res.Diff, res.Added, res.Removed, res.DiffNote)
		}
	})

	t.Run("a rename through a long file keeps its diff", func(t *testing.T) {
		calls := slices.Clone(lines)
		for _, n := range []int{10, 20000, 39990} {
			calls[n-1] = "call name()\n"
		}
		// Each replacement adds a line, so each later hunk starts one line
		// further on in the new file than in the old.
		res := runEdit(t, strings.Join(calls, ""), FileOpRequest{OldText: "name()", NewText: "renamed(\n\targ)", ReplaceAll: true})
		for _, header := range []string{"@@ -7,7 +7,8 @@\n", "@@ -19997,7 +19998,8 @@\n", "@@ -39987,7 +39989,8 @@\n"} {
			if !strings.Contains(res.Diff, header) {
				t.Errorf("diff = %q, want hunk %q", res.Diff, header)
			}
		}
		if res.ReplacedCount != 3 || res.Added != 6 || res.Removed != 3 || res.DiffNote != "" || strings.Count(res.Diff, "-call name()\n+call renamed(\n+\targ)\n") != 3 {
			t.Errorf("count %d, +%d/-%d, note %q, diff %q; want 3 replacements shown", res.ReplacedCount, res.Added, res.Removed, res.DiffNote, res.Diff)
		}
	})
}

// An edit whose replacements, with their context, come to over the line or
// size cap gets a note in place of the diff, and is still applied in full.
func TestFileOp_EditDiffLimits(t *testing.T) {
	lines := numberedLines()

	t.Run("replace_all through the whole file", func(t *testing.T) {
		alt := make([]string, len(lines))
		for i, l := range lines {
			if i%2 == 1 {
				l = "same\n"
			}
			alt[i] = l
		}
		old := strings.Join(alt, "")
		want := strings.ReplaceAll(old, "same\n", "SAME\n")
		res := runEdit(t, old, FileOpRequest{OldText: "same\n", NewText: "SAME\n", ReplaceAll: true})
		if res.Diff != "" || res.DiffNote != "no diff: the changes, with context, are over 2000 lines" || res.Added != 0 || res.Removed != 0 {
			t.Errorf("result = diff %d bytes (+%d/-%d), note %q; want no diff and the line note", len(res.Diff), res.Added, res.Removed, res.DiffNote)
		}
		if res.ReplacedCount != 20000 || res.OldSHA256 != sha256Hex([]byte(old)) || res.SHA256 != sha256Hex([]byte(want)) {
			t.Errorf("count %d, old %s, new %s; want 20000 and both hashes", res.ReplacedCount, res.OldSHA256, res.SHA256)
		}
	})

	for _, tc := range []struct {
		name, content, new string
		count              int
	}{
		{"one line over 256 KB", strings.Repeat("a", 300*1024) + "\n", "b", 300 * 1024},
		{"1,000 lines of 300 bytes", strings.Repeat(strings.Repeat("a", 299)+"\n", 1000), "b", 299000},
		{"lines grown past 256 KB", strings.Repeat("a\n", 300), strings.Repeat("b", 1024), 300},
	} {
		t.Run("over 256 KB: "+tc.name, func(t *testing.T) {
			res := runEdit(t, tc.content, FileOpRequest{OldText: "a", NewText: tc.new, ReplaceAll: true})
			if res.Diff != "" || res.DiffNote != "no diff: the changes, with context, are over 256 KB" || res.ReplacedCount != tc.count {
				t.Errorf("result = diff %d bytes, note %q, count %d; want the size note", len(res.Diff), res.DiffNote, res.ReplacedCount)
			}
		})
	}

	t.Run("the line cap counts a last line without a newline", func(t *testing.T) {
		for _, tc := range []struct {
			lines int
			note  string
		}{{2000, ""}, {2001, "no diff: the changes, with context, are over 2000 lines"}} {
			content := strings.Repeat("x\n", tc.lines-1) + "x"
			res := runEdit(t, content, FileOpRequest{OldText: "x", NewText: "y", ReplaceAll: true})
			if res.DiffNote != tc.note || (tc.note == "") != (res.Diff != "") {
				t.Errorf("%d lines: note %q, diff %d bytes; want note %q", tc.lines, res.DiffNote, len(res.Diff), tc.note)
			}
		}
	})
}

// Non-UTF-8 lines get a note, and a long diff is cut with the edit's marker.
func TestFileOp_EditDiffOfBinaryAndLongEdits(t *testing.T) {
	t.Run("binary content", func(t *testing.T) {
		for _, tc := range []struct{ old, new string }{{"\xff", "\xfe"}, {"\xff", "-"}, {"-", "\xff"}} {
			content := "text\nA" + tc.old + "B\n"
			res := runEdit(t, content, FileOpRequest{OldText: tc.old, NewText: tc.new})
			if res.Diff != "" || res.DiffNote != "no diff: binary content" || res.SHA256 != sha256Hex([]byte("text\nA"+tc.new+"B\n")) {
				t.Errorf("%q → %q: diff %q, note %q; want the binary note", tc.old, tc.new, res.Diff, res.DiffNote)
			}
		}
	})

	t.Run("a long diff is cut, and says the edit applied", func(t *testing.T) {
		items := make([]string, 1000)
		for i := range items {
			items[i] = fmt.Sprintf("item %04d\n", i)
		}
		res := runEdit(t, strings.Join(items, ""), FileOpRequest{OldText: "item", NewText: "entry", ReplaceAll: true})
		if len(res.Diff) > 4096+100 || !strings.HasSuffix(res.Diff, "diff truncated (edit applied in full) ...") || res.Added != 1000 || res.Removed != 1000 {
			t.Errorf("diff of %d bytes (+%d/-%d), ending %q; want it cut near 4 KB with the edit note", len(res.Diff), res.Added, res.Removed, res.Diff[max(0, len(res.Diff)-80):])
		}
	})
}

// An edit's diff, cut from the lines around each replacement, is the diff
// difflib gives for the whole file: hunks just apart or just close enough to
// merge, a replacement in the context of the one before, two on one line,
// text across lines, added and removed lines, the first and last lines,
// CRLF, a pattern that overlaps itself, form feeds, and a window that starts
// on a blank line. Every diff must also apply to the old file and give the
// new one.
func TestFileOp_EditDiffMatchesTheWholeFileDiff(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "fileops.py"), fileOpsScript, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `import re, sys
sys.path.insert(0, sys.argv[1])
import fileops
base = "".join("line %d\n" % i for i in range(1, 41))
def at(*lines):
    rows = ["line %d\n" % i for i in range(1, 41)]
    for n in lines:
        rows[n - 1] = "call f(x) here\n"
    return "".join(rows)
cases = [
    ("one", base, "line 20\n", "changed\n", False),
    ("seven apart", at(10, 17), "f(x)", "g(x)", True),
    ("eight apart", at(10, 18), "f(x)", "g(x)", True),
    ("in the context before", at(10, 12), "f(x)", "g(x)", True),
    ("two on one line", base.replace("line 20\n", "f(x) f(x)\n"), "f(x)", "g(y)", True),
    ("across lines", base, "line 5\nline 6\n", "joined\n", False),
    ("adds lines", at(3, 30), "f(x)", "f(\n  x,\n)", True),
    ("removes lines", at(3, 30), "call f(x) here\n", "", True),
    ("first line", base, "line 1\n", "first\n", False),
    ("last line", base, "line 40\n", "last\n", False),
    ("crlf", base.replace("\n", "\r\n"), "line 7", "seven", False),
    ("overlapping", "aaaaa\n" + base, "aa", "b", True),
    ("form feeds", "x\fy\n" + base, "line 3\n", "three\n", False),
    ("a window from a blank line", "".join("\n" if i % 2 else "line %d\n" % i for i in range(1, 41)), "line 20\n", "twenty\n", False),
]
HDR = re.compile(r"@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")
def apply(old, diff):
    lines = fileops._lines(diff)[2:]
    out, pos, i = [], 0, 0
    while i < len(lines):
        m = HDR.match(lines[i])
        i += 1
        a, b = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
        c, d = int(m.group(3)), int(m.group(4) if m.group(4) is not None else 1)
        start = a - 1 if b else a
        if start < pos or (c - 1 if d else c) != len(out) + start - pos:
            return None
        out += old[pos:start]
        pos = start
        while i < len(lines) and not lines[i].startswith("@@"):
            tag, text = lines[i][0], lines[i][1:]
            i += 1
            if tag in " -":
                if pos >= len(old) or old[pos] != text:
                    return None
                pos += 1
            if tag in " +":
                out.append(text)
    return out + old[pos:]
bad = []
for name, text, old, new, every in cases:
    before, o, n = text.encode(), old.encode(), new.encode()
    count = before.count(o) if every else 1
    after = before.replace(o, n) if every else before.replace(o, n, 1)
    whole = fileops._bounded_diff("f", before, after, "edit")
    got = fileops._edit_diff("f", before, after, o, n, count)
    if got != whole:
        bad.append("%s:\n%r\n%r" % (name, whole, got))
    if apply(fileops._lines(text), got[0]) != fileops._lines(after.decode()):
        bad.append("%s: the diff does not apply:\n%r" % (name, got))
print("\n".join(bad) or "ok")
`
	out, err := exec.Command("python3", "-I", "-c", driver, mod).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("edit diffs differ from whole-file diffs (err %v):\n%s", err, out)
	}
}

// Hunk headers count lines at "\n", as grep -n and sed do: a form feed, a
// vertical tab or a lone \r inside a line does not start a new one.
func TestFileOp_DiffLinesAreCountedAtNewlines(t *testing.T) {
	sb := fileopTestSandbox(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	old := "a\fb\vc\rd\nline 2\nline 3\nline 4\nline 5\nline 6\n"
	for _, req := range []FileOpRequest{
		{Op: FileOpEdit, OldText: "line 6", NewText: "six"},
		{Op: FileOpWrite, Data: []byte(strings.Replace(old, "line 6", "six", 1))},
	} {
		if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		req.Path, req.Root = path, root
		res, err := sb.RunFileOp(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Diff, "@@ -3,4 +3,4 @@\n line 3\n line 4\n line 5\n-line 6\n+six\n") {
			t.Errorf("op %v: diff = %q, want line 6 numbered as the sixth line", req.Op, res.Diff)
		}
	}
}

// An edit whose file changes between the read and the rename is refused as
// stale, and the concurrent change is kept: the reply built before the commit
// is not sent.
func TestFileOp_GuardedEditRechecksBeforeTheRename(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := `real_diff = fileops._edit_diff
def diff_then_change(path, *args):
    result = real_diff(path, *args)
    with open(path, "wb") as f:
        f.write(b"concurrent\n")
    return result
fileops._edit_diff = diff_then_change
`
	_, err := runPatchedOp(t, "edit", map[string]any{
		"path": path, "root": root, "old_b64": "b2xk", "new_b64": "bmV3", // old → new
		"expected_sha256": sha256Hex([]byte("old\n")),
	}, patch)
	if !errors.Is(err, ErrFileOpStale) || !strings.Contains(err.Error(), "current "+sha256Hex([]byte("concurrent\n"))) {
		t.Fatalf("err = %v, want stale naming the concurrent content's hash", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "concurrent\n" {
		t.Fatalf("the concurrent change was overwritten: %q", got)
	}
}

// The diff describes the file as it was before the rename: work done after
// the commit could outlast the file-op timeout and report an edit or a write
// that landed as failed.
func TestFileOp_DiffIsTakenBeforeTheCommit(t *testing.T) {
	for op, req := range map[string]map[string]any{
		"write": {"data_b64": "bmV3Cg=="},               // new\n
		"edit":  {"old_b64": "b2xk", "new_b64": "bmV3"}, // old → new
	} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "f.txt")
			if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			req["path"], req["root"] = path, root
			patch := `def hunks_of_the_file_on_disk(before, after, old_offset, new_offset):
    with open(` + strconvQuote(path) + `, "rb") as f:
        return [" on disk: " + f.read().decode()]
fileops._hunks = hunks_of_the_file_on_disk
`
			res, err := runPatchedOp(t, op, req, patch)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(res.Diff, "\n on disk: old\n") {
				t.Errorf("diff saw %q, want the file before the rename", res.Diff)
			}
			if got, _ := os.ReadFile(path); string(got) != "new\n" {
				t.Errorf("file = %q, want new", got)
			}
		})
	}
}

// The windowed diff is the diff difflib gives for the whole file, for
// changes at the start, middle and end, insertions, deletions, and a file
// without a final newline. Where a change lands in a run of identical lines
// the two may place it differently, so there each diff is applied to the old
// file instead, and must give the new one.
func TestFileOp_WriteDiffWindowMatchesTheWholeFileDiff(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "fileops.py"), fileOpsScript, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `import sys
sys.path.insert(0, sys.argv[1])
import fileops
base = ["line %d\n" % i for i in range(1, 41)]
cases = {
    "first": ["changed\n"] + base[1:],
    "last": base[:-1] + ["changed\n"],
    "middle": base[:20] + ["changed\n"] + base[21:],
    "two near": base[:10] + ["x\n"] + base[11:15] + ["y\n"] + base[16:],
    "two far": base[:5] + ["x\n"] + base[6:30] + ["y\n"] + base[31:],
    "insert": base[:12] + ["new\n", "new 2\n"] + base[12:],
    "delete": base[:12] + base[15:],
    "no final newline": base[:-1] + ["line 40"],
    "all": ["other %d\n" % i for i in range(1, 41)],
    "empty": [],
}
import re
HDR = re.compile(r"@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@")
def apply(old, diff):
    lines = diff.splitlines(keepends=True)[2:]
    out, pos, i = [], 0, 0
    while i < len(lines):
        m = HDR.match(lines[i])
        i += 1
        a, b = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
        start = a - 1 if b else a
        if start < pos:
            return None
        out += old[pos:start]
        pos = start
        while i < len(lines) and not lines[i].startswith("@@"):
            tag, text = lines[i][0], lines[i][1:]
            i += 1
            if tag in " -":
                if pos >= len(old) or old[pos] != text:
                    return None
                pos += 1
            if tag in " +":
                out.append(text)
    return out + old[pos:]
blank = ["\n"] * 10
runs = {
    "a line added to a run": (blank, blank + ["\n"]),
    "a line taken from a run": (blank, blank[1:]),
    "a run grown in the middle": (base[:5] + blank + base[5:], base[:5] + blank * 2 + base[5:]),
}
bad = []
for name, new in cases.items():
    old_b, new_b = "".join(base).encode(), "".join(new).encode()
    whole = fileops._bounded_diff("f", old_b, new_b, "write")
    window = fileops._bounded_diff("f", old_b, new_b, "write", 2000)
    if whole != window:
        bad.append("%s:\n%r\n%r" % (name, whole, window))
for name, new in cases.items():
    window = fileops._bounded_diff("f", "".join(base).encode(), "".join(new).encode(), "write", 2000)
    if apply(base, window[0]) != new:
        bad.append("%s: the windowed diff does not apply:\n%r" % (name, window))
for name, (old, new) in runs.items():
    old_b, new_b = "".join(old).encode(), "".join(new).encode()
    whole = fileops._bounded_diff("f", old_b, new_b, "write")
    window = fileops._bounded_diff("f", old_b, new_b, "write", 2000)
    if apply(old, window[0]) != new or window[1:] != whole[1:]:
        bad.append("%s: the windowed diff does not apply, or its counts differ:\n%r\n%r" % (name, whole, window))
print("\n".join(bad) or "ok")
`
	out, err := exec.Command("python3", "-I", "-c", driver, mod).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("windowed diffs differ from whole-file diffs (err %v):\n%s", err, out)
	}
}
