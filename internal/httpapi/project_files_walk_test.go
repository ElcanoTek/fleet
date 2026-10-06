package httpapi

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeWalkFixture writes n files into convID's workspace, file i modified i
// seconds after a fixed base, plus an upload that must never be listed.
func writeWalkFixture(t *testing.T, convID string, n int) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	ws := filepath.Join(root, convID)
	base := time.Unix(1_700_000_000, 0)
	for i := range n {
		p := filepath.Join(ws, "d", fmt.Sprintf("f%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	up := filepath.Join(ws, uploadsDir, "u", "in.pdf")
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(up, []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// More files than the cap: only the newest `limit` are kept, newest first,
// and the listing says it was truncated.
func TestWalkWorkspaceFilesKeepsNewestBoundedSet(t *testing.T) {
	const n = maxProjectFiles + 57
	writeWalkFixture(t, "c1", n)
	got, truncated := walkWorkspaceFiles("c1", maxProjectFiles)
	if !truncated {
		t.Error("more files than the cap must report truncated")
	}
	if len(got) != maxProjectFiles {
		t.Fatalf("len = %d, want %d", len(got), maxProjectFiles)
	}
	for i, f := range got {
		want := fmt.Sprintf("d/f%04d.txt", n-1-i)
		if f.Path != want {
			t.Fatalf("got[%d] = %s, want %s (newest first)", i, f.Path, want)
		}
	}

	// Exactly at the cap: everything, not truncated; uploads never listed.
	writeWalkFixture(t, "c2", 5)
	got, truncated = walkWorkspaceFiles("c2", 5)
	if truncated || len(got) != 5 || got[0].Path != "d/f0004.txt" || got[4].Path != "d/f0000.txt" {
		t.Errorf("at cap: %v truncated=%v", got, truncated)
	}

	// No workspace at all: empty, not truncated.
	if got, truncated := walkWorkspaceFiles("nope", 5); len(got) != 0 || truncated {
		t.Errorf("missing workspace: %v %v", got, truncated)
	}
}

// Ties on modtime break by path, as the old full sort did.
func TestWalkWorkspaceFilesTieOrder(t *testing.T) {
	writeWalkFixture(t, "c", 0)
	ws := filepath.Join(os.Getenv("FLEET_WORKSPACE_ROOT"), "c")
	mt := time.Unix(1_700_000_000, 0)
	for _, name := range []string{"b.txt", "a.txt", "c.txt", "d.txt"} {
		p := filepath.Join(ws, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated := walkWorkspaceFiles("c", 3)
	if !truncated || len(got) != 3 || got[0].Path != "a.txt" || got[1].Path != "b.txt" || got[2].Path != "c.txt" {
		t.Errorf("tie order = %v truncated=%v", got, truncated)
	}
}

// The walk itself is bounded: past the visit budget it stops and reports
// truncated rather than statting a runaway tree whole.
func TestWalkWorkspaceFilesVisitBudget(t *testing.T) {
	writeWalkFixture(t, "c", 50)
	old := maxWorkspaceWalkEntries
	maxWorkspaceWalkEntries = 20
	t.Cleanup(func() { maxWorkspaceWalkEntries = old })
	got, truncated := walkWorkspaceFiles("c", maxProjectFiles)
	if !truncated {
		t.Error("hitting the visit budget must report truncated")
	}
	if len(got) == 0 || len(got) >= 20 {
		t.Errorf("visited past the budget: %d files", len(got))
	}
}
