package sharedfiles

// Load-bearing assertions: name/folder sanitization rejects (not repairs)
// unusable input, staging round-trips bytes into the manifest-derived path,
// and Sync makes the staged tree converge to the manifest from every drift
// direction — missing file, wrong-sized file, stray file, stray empty folder.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/store"
)

func testLibrary(t *testing.T) Library {
	t.Helper()
	return New(t.TempDir(), t.TempDir())
}

func saveFile(t *testing.T, l Library, f store.SharedFile, content string) store.SharedFile {
	t.Helper()
	size, sha, err := l.SaveCanonical(f.ID, strings.NewReader(content))
	if err != nil {
		t.Fatalf("SaveCanonical: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("SaveCanonical size = %d, want %d", size, len(content))
	}
	if sha == "" {
		t.Fatalf("SaveCanonical returned empty sha256")
	}
	f.SizeBytes = size
	f.SHA256 = sha
	return f
}

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "report.csv", want: "report.csv"},
		{in: "  q3 data (final).xlsx ", want: "q3 data (final).xlsx"},
		{in: `..\..\evil.sh`, want: "evil.sh"},
		{in: "/etc/passwd", want: "passwd"},
		{in: ".hidden", want: "hidden"},
		{in: "a:b*c?.txt", want: "a_b_c_.txt"},
		{in: "...", wantErr: true},
		{in: "", wantErr: true},
		{in: "\x00\x01", wantErr: true},
	}
	for _, c := range cases {
		got, err := SanitizeName(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("SanitizeName(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A long non-ASCII name is capped on a RUNE boundary: a byte slice through a
// multi-byte character produced invalid UTF-8, which Postgres rejected — so
// the upload 500'd instead of landing under a shortened name.
func TestSanitizeNameCapsAtRuneBoundary(t *testing.T) {
	// 'é' is 2 bytes; 150 of them = 300 bytes, over maxNameLen (200) with the
	// cut landing mid-rune at any odd offset.
	got, err := SanitizeName(strings.Repeat("é", 150) + ".csv")
	if err != nil {
		t.Fatalf("SanitizeName: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("SanitizeName produced invalid UTF-8: %q", got)
	}
	if len(got) > maxNameLen {
		t.Errorf("len = %d bytes, want <= %d", len(got), maxNameLen)
	}
	if !strings.HasSuffix(got, ".csv") {
		t.Errorf("extension dropped: %q", got)
	}
}

func TestSanitizeFolder(t *testing.T) {
	if got, err := SanitizeFolder("  "); err != nil || got != "" {
		t.Errorf("blank folder = (%q, %v), want root", got, err)
	}
	if got, err := SanitizeFolder("historical"); err != nil || got != "historical" {
		t.Errorf("plain folder = (%q, %v)", got, err)
	}
	// Nested-looking folders are rejected, not silently flattened.
	if _, err := SanitizeFolder("a/b"); err == nil {
		t.Errorf("nested folder accepted")
	}
	if _, err := SanitizeFolder(`a\b`); err == nil {
		t.Errorf("backslash folder accepted")
	}
	if _, err := SanitizeFolder(strings.Repeat("x", 100)); err == nil {
		t.Errorf("overlong folder accepted")
	}
}

func TestStageUnstageRoundTrip(t *testing.T) {
	l := testLibrary(t)
	f := saveFile(t, l, store.SharedFile{ID: "abc123", Name: "data.csv", Folder: "q3"}, "a,b\n1,2\n")
	if err := l.Stage(f); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	staged, err := l.StagedPath(f)
	if err != nil {
		t.Fatalf("StagedPath: %v", err)
	}
	if want := filepath.Join(l.StagedRoot, "q3", "data.csv"); staged != want {
		t.Fatalf("StagedPath = %q, want %q", staged, want)
	}
	got, err := os.ReadFile(staged)
	if err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("staged content = (%q, %v)", got, err)
	}
	if err := l.Unstage(f); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged file survived Unstage: %v", err)
	}
	// The now-empty folder is pruned too.
	if _, err := os.Stat(filepath.Join(l.StagedRoot, "q3")); !os.IsNotExist(err) {
		t.Fatalf("empty folder survived Unstage: %v", err)
	}
	// Unstaging again is a no-op, not an error.
	if err := l.Unstage(f); err != nil {
		t.Fatalf("second Unstage: %v", err)
	}
}

func TestPromptPath(t *testing.T) {
	if got := PromptPath(store.SharedFile{Name: "a.csv"}); got != "shared/a.csv" {
		t.Errorf("root PromptPath = %q", got)
	}
	if got := PromptPath(store.SharedFile{Name: "a.csv", Folder: "hist"}); got != "shared/hist/a.csv" {
		t.Errorf("folder PromptPath = %q", got)
	}
}

// TestPromptBlock pins the one announcement renderer both drivers use
// (#1301): empty library renders nothing, entries carry the shared/ path +
// human size + optional description, and the cap degrades to an enumeration
// hint instead of flooding the prompt.
func TestPromptBlock(t *testing.T) {
	if got := PromptBlock(nil); got != "" {
		t.Fatalf("empty-library PromptBlock = %q, want empty", got)
	}

	files := []store.SharedFile{
		{Name: "a.csv", SizeBytes: 8, Description: "history"},
		{Name: "b.csv", Folder: "q3", SizeBytes: 2048},
	}
	block := PromptBlock(files)
	for _, want := range []string{
		"**Shared file library**",
		"- `shared/a.csv` (8 B) — history\n",
		"- `shared/q3/b.csv` (2.0 KB)\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("PromptBlock missing %q in %q", want, block)
		}
	}

	many := make([]store.SharedFile, MaxPromptEntries+3)
	for i := range many {
		many[i] = store.SharedFile{Name: fmt.Sprintf("f%03d.txt", i), SizeBytes: 1}
	}
	block = PromptBlock(many)
	if want := "…and 3 more"; !strings.Contains(block, want) {
		t.Errorf("overflow PromptBlock missing %q", want)
	}
	if strings.Count(block, "- `shared/") != MaxPromptEntries {
		t.Errorf("overflow PromptBlock lists %d entries, want %d", strings.Count(block, "- `shared/"), MaxPromptEntries)
	}
}

func TestUnsafeManifestRowsAreRefused(t *testing.T) {
	l := testLibrary(t)
	// A hand-edited DB row must never turn staging into a traversal.
	for _, f := range []store.SharedFile{
		{ID: "ok", Name: "../escape", Folder: ""},
		{ID: "ok", Name: "x", Folder: ".."},
		{ID: "../escape", Name: "x"},
	} {
		if _, err := l.StagedPath(f); f.ID == "ok" && err == nil {
			t.Errorf("StagedPath accepted unsafe row %+v", f)
		}
		if err := l.Stage(f); err == nil {
			t.Errorf("Stage accepted unsafe row %+v", f)
		}
		// Unstage is the destructive primitive: it os.Removes a path derived
		// from DB state, so StagedPath is the only thing between a hand-edited
		// row and an arbitrary unlink. Stage and StagedPath were asserted
		// here; the delete was not. Gated on the id like StagedPath above,
		// because Unstage derives its path from name+folder only — an unsafe
		// ID has nothing to reject (it never reaches canonicalPath).
		if err := l.Unstage(f); f.ID == "ok" && err == nil {
			t.Errorf("Unstage accepted unsafe row %+v", f)
		}
	}
	if _, _, err := l.SaveCanonical("../escape", strings.NewReader("x")); err == nil {
		t.Errorf("SaveCanonical accepted unsafe id")
	}
}

func TestSyncConverges(t *testing.T) {
	l := testLibrary(t)
	a := saveFile(t, l, store.SharedFile{ID: "ida", Name: "a.txt"}, "aaaa")
	b := saveFile(t, l, store.SharedFile{ID: "idb", Name: "b.txt", Folder: "sub"}, "bbbb")
	manifest := []store.SharedFile{a, b}

	// Drift, one of each kind: a missing, b wrong-sized, plus a stray file and
	// a stray empty dir.
	if err := os.MkdirAll(filepath.Join(l.StagedRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.StagedRoot, "sub", "b.txt"), []byte("tampered!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.StagedRoot, "stray.txt"), []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.StagedRoot, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := l.Sync(manifest); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(l.StagedRoot, "a.txt")); err != nil || string(got) != "aaaa" {
		t.Errorf("missing file not restaged: (%q, %v)", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(l.StagedRoot, "sub", "b.txt")); err != nil || string(got) != "bbbb" {
		t.Errorf("wrong-sized file not restaged: (%q, %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(l.StagedRoot, "stray.txt")); !os.IsNotExist(err) {
		t.Errorf("stray file survived Sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.StagedRoot, "empty-dir")); !os.IsNotExist(err) {
		t.Errorf("stray empty dir survived Sync: %v", err)
	}

	// A second pass over a converged tree is a no-op that still succeeds.
	if err := l.Sync(manifest); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	// Sync with an empty manifest clears the tree but keeps the root.
	if err := l.Sync(nil); err != nil {
		t.Fatalf("empty Sync: %v", err)
	}
	entries, err := os.ReadDir(l.StagedRoot)
	if err != nil {
		t.Fatalf("staged root gone after empty Sync: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staged root not empty after empty Sync: %v", entries)
	}
}

func TestRemoveCanonical(t *testing.T) {
	l := testLibrary(t)
	f := saveFile(t, l, store.SharedFile{ID: "gone", Name: "g.txt"}, "g")
	if err := l.RemoveCanonical(f.ID); err != nil {
		t.Fatalf("RemoveCanonical: %v", err)
	}
	if err := l.RemoveCanonical(f.ID); err != nil {
		t.Fatalf("RemoveCanonical (absent): %v", err)
	}
	if err := l.Stage(f); err == nil {
		t.Fatalf("Stage after RemoveCanonical should fail")
	}
}

func TestTotalBytes(t *testing.T) {
	files := []store.SharedFile{{SizeBytes: 3}, {SizeBytes: 39}}
	if got := TotalBytes(files); got != 42 {
		t.Errorf("TotalBytes = %d, want 42", got)
	}
}

// TestUnstageKeepsFolderWithSurvivingSibling pins the direction the folder
// prune must NOT go. Unstage's os.Remove of the folder is best-effort and
// relies on ENOTEMPTY being swallowed; a refactor to RemoveAll would read as
// equivalent and would silently delete another admin's shared file. Only the
// empty-folder prune was covered.
func TestUnstageKeepsFolderWithSurvivingSibling(t *testing.T) {
	l := testLibrary(t)
	a := saveFile(t, l, store.SharedFile{ID: "ida", Name: "a.csv", Folder: "q3"}, "aaaa")
	b := saveFile(t, l, store.SharedFile{ID: "idb", Name: "b.csv", Folder: "q3"}, "bbbb")
	for _, f := range []store.SharedFile{a, b} {
		if err := l.Stage(f); err != nil {
			t.Fatalf("stage %s: %v", f.ID, err)
		}
	}

	if err := l.Unstage(a); err != nil {
		t.Fatalf("unstage a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.StagedRoot, "q3", "a.csv")); !os.IsNotExist(err) {
		t.Fatalf("a.csv survived unstage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(l.StagedRoot, "q3", "b.csv"))
	if err != nil || string(got) != "bbbb" {
		t.Fatalf("sibling b.csv = (%q, %v); the folder prune must not touch it", got, err)
	}

	// Once the last file goes, the folder does too.
	if err := l.Unstage(b); err != nil {
		t.Fatalf("unstage b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.StagedRoot, "q3")); !os.IsNotExist(err) {
		t.Fatalf("emptied folder survived: %v", err)
	}
}

// TestSyncIsBestEffortPerRow pins the invariant Sync's doc comment states and
// nothing exercised: one bad row must not stop the rest of the library from
// healing. Asserts only that an error came back, never which one — note()
// keeps the first, and the map it iterates has no order.
func TestSyncIsBestEffortPerRow(t *testing.T) {
	l := testLibrary(t)
	good := saveFile(t, l, store.SharedFile{ID: "idgood", Name: "good.csv"}, "good")
	// Row whose canonical bytes are gone: fails inside Stage's open.
	missing := saveFile(t, l, store.SharedFile{ID: "idgone", Name: "gone.csv"}, "gone")
	if err := l.RemoveCanonical(missing.ID); err != nil {
		t.Fatalf("RemoveCanonical: %v", err)
	}
	// Row that fails earlier still, in stagedRel.
	unsafe := store.SharedFile{ID: "idunsafe", Name: "../escape"}

	err := l.Sync([]store.SharedFile{missing, unsafe, good})
	if err == nil {
		t.Fatalf("Sync must report the first failure, got nil")
	}
	staged, readErr := os.ReadFile(filepath.Join(l.StagedRoot, "good.csv"))
	if readErr != nil || string(staged) != "good" {
		t.Fatalf("good row = (%q, %v); a bad row must not stop the healthy ones", staged, readErr)
	}
}

// errReader fails partway through, so the copy inside SaveCanonical breaks
// after bytes have already landed in the temp file.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestSaveCanonicalMidStreamFailureLeavesNothing pins SaveCanonical's
// crash-safety claim. It matters more than it looks: Sync reconciles only the
// staged tree, so nothing ever sweeps strays out of CanonicalDir — a leaked
// .upload-* temp there is permanent.
func TestSaveCanonicalMidStreamFailureLeavesNothing(t *testing.T) {
	l := testLibrary(t)
	r := io.MultiReader(strings.NewReader("abc"), errReader{})
	if _, _, err := l.SaveCanonical("idpartial", r); err == nil {
		t.Fatalf("SaveCanonical accepted a failing reader")
	}
	if _, err := os.Stat(filepath.Join(l.CanonicalDir, "idpartial")); !os.IsNotExist(err) {
		t.Fatalf("partial canonical file was published: %v", err)
	}
	entries, err := os.ReadDir(l.CanonicalDir)
	if err != nil {
		t.Fatalf("read canonical dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("leaked staging temp %q — nothing ever sweeps CanonicalDir", e.Name())
		}
	}
}

// TestSaveCanonicalReturnsTrueDigest asserts the exact hash rather than
// "non-empty", which is what every other assertion on this value settles for —
// a hasher fed the wrong stream, or none, passes those.
func TestSaveCanonicalReturnsTrueDigest(t *testing.T) {
	l := testLibrary(t)
	const body = "a,b\n1,2\n"
	sum := sha256.Sum256([]byte(body))
	n, sha, err := l.SaveCanonical("iddigest", strings.NewReader(body))
	if err != nil {
		t.Fatalf("SaveCanonical: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("size = %d, want %d", n, len(body))
	}
	if want := hex.EncodeToString(sum[:]); sha != want {
		t.Fatalf("sha256 = %q, want %q", sha, want)
	}
}
