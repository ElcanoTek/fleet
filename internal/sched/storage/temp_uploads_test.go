package storage

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
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
