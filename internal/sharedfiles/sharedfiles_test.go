package sharedfiles

// Load-bearing assertions: name/folder sanitization rejects (not repairs)
// unusable input, staging round-trips bytes into the manifest-derived path,
// and Sync makes the staged tree converge to the manifest from every drift
// direction — missing file, wrong-sized file, stray file, stray empty folder.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
