package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDuTree_SumsRegularFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.bin"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}

	got := duTree(context.Background(), dir)
	if got.Bytes != 150 {
		t.Errorf("Bytes = %d, want 150", got.Bytes)
	}
	if got.Files != 2 {
		t.Errorf("Files = %d, want 2", got.Files)
	}
}

func TestDuTree_MissingDirIsZeroNotError(t *testing.T) {
	got := duTree(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if got.Bytes != 0 || got.Files != 0 {
		t.Errorf("missing dir should account as empty, got %+v", got)
	}
}

func TestSweepTempUploads_RemovesOldKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "old.bin")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "new.bin")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed := sweepTempUploads(dir, 24*time.Hour)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale file must be gone")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file must survive")
	}
}
