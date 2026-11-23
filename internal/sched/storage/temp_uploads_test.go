package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTempUploadKeeperLookupsDoNotGrowWithKeptFiles: however many aged files
// a live job names, one sweep loads the live names once and asks about a file
// on its own only when it is about to be removed.
func TestTempUploadKeeperLookupsDoNotGrowWithKeptFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := map[string]struct{}{}
	for i := range 1000 {
		live[fmt.Sprintf("kept_%04d.txt", i)] = struct{}{}
	}
	snapshots, rechecks := 0, 0
	keep := newTempUploadKeeper(dir,
		func() (map[string]struct{}, error) { snapshots++; return live, nil },
		func(string) (bool, error) { rechecks++; return false, nil })

	for name := range live {
		for _, rel := range []string{name, ".checksums/" + name + ".sha256"} {
			inUse, err := keep(filepath.Join(dir, rel))
			if err != nil || !inUse {
				t.Fatalf("keep(%s) = %v, %v; want kept", rel, inUse, err)
			}
		}
	}
	for i := range 3 {
		inUse, err := keep(filepath.Join(dir, fmt.Sprintf("unused_%d.txt", i)))
		if err != nil || inUse {
			t.Fatalf("an unnamed file reads as kept (%v, %v)", inUse, err)
		}
	}
	if snapshots != 1 || rechecks != 3 {
		t.Errorf("lookups: %d snapshots and %d single-file rechecks for 2000 kept and 3 removable files; want 1 and 3", snapshots, rechecks)
	}
}

// TestTempUploadKeeperKeepsEverythingWhenTheLookupFails: a failed snapshot is
// not a "nothing is named"; every file stays.
func TestTempUploadKeeperKeepsEverythingWhenTheLookupFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lookupErr := errors.New("database unavailable")
	keep := newTempUploadKeeper(dir,
		func() (map[string]struct{}, error) { return nil, lookupErr },
		func(string) (bool, error) { t.Fatal("no recheck after a failed snapshot"); return false, nil })
	if inUse, err := keep(filepath.Join(dir, "brief.txt")); !inUse || !errors.Is(err, lookupErr) {
		t.Errorf("keep = %v, %v; want kept with the lookup error", inUse, err)
	}
}

// TestSweepTempUploadsRemovesSidecarsWithTheirUpload: an upload goes with its
// .checksums and .uploaders sidecars and is counted once; a kept upload, and
// one not yet aged, keep theirs whatever the sidecars' own age; a sidecar
// whose upload is already gone is removed once aged; a fresh orphan stays.
func TestSweepTempUploadsRemovesSidecarsWithTheirUpload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	write := func(rel string, mtime time.Time) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	withSidecars := func(name string, upload, sidecar time.Time) []string {
		return []string{write(name, upload), write(".checksums/"+name+".sha256", sidecar), write(".uploaders/"+name, sidecar)}
	}
	kept := withSidecars("kept_1a2b3c4d.txt", old, old)
	fresh := withSidecars("fresh_2b3c4d5e.txt", time.Now(), old)
	gone := withSidecars("gone_3c4d5e6f.txt", old, time.Now())
	orphanOld := []string{write(".checksums/orphan_4d5e6f7a.txt.sha256", old), write(".uploaders/orphan_4d5e6f7a.txt", old)}
	orphanFresh := write(".uploaders/orphan_5e6f7a8b.txt", time.Now())

	keep := func(path string) (bool, error) { return filepath.Base(path) == "kept_1a2b3c4d.txt", nil }
	got := SweepTempUploadsWith(dir, 14*24*time.Hour, keep, nil)
	if got.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (the one upload; sidecars are not counted)", got.Removed)
	}
	for _, p := range append(append(append([]string{}, kept...), fresh...), orphanFresh) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	for _, p := range append(append([]string{}, gone...), orphanOld...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was left behind (stat err %v)", p, err)
		}
	}
}

// TestSweepTempUploadsByAgeAlone: with no keep rule, aged uploads go and fresh
// ones stay.
func TestSweepTempUploadsByAgeAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale, fresh := filepath.Join(dir, "old.bin"), filepath.Join(dir, "new.bin")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	if got := SweepTempUploadsWith(dir, 24*time.Hour, nil, nil); got.Removed != 1 {
		t.Errorf("Removed = %d, want 1", got.Removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the aged upload stayed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the fresh upload was removed")
	}
}
