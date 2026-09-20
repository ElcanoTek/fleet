package handlers

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupTempFilesMissingDirIsQuiet: a box that has never received an
// upload has no temp_uploads directory at all. That is "nothing to clean",
// not an error — the maintenance loop runs hourly, so logging it surfaced a
// spurious ERROR every hour on exactly those boxes.
func TestCleanupTempFilesMissingDirIsQuiet(t *testing.T) {
	h := &Handlers{config: Config{DataDir: t.TempDir()}}

	var buf bytes.Buffer
	restore := captureLog(t, &buf)
	defer restore()

	h.CleanupTempFiles(time.Hour)

	if buf.Len() != 0 {
		t.Errorf("missing temp_uploads should log nothing, got: %q", buf.String())
	}
}

// TestCleanupTempFilesRemovesAgedFile: the quiet path above must not disable
// the real sweep — an aged upload still gets reclaimed when the directory
// exists, and a fresh one survives.
func TestCleanupTempFilesRemovesAgedFile(t *testing.T) {
	dataDir := t.TempDir()
	tempDir := filepath.Join(dataDir, "temp_uploads")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aged := filepath.Join(tempDir, "brief_old.txt")
	if err := os.WriteFile(aged, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(tempDir, "brief_new.txt")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}

	h := &Handlers{config: Config{DataDir: dataDir}}
	h.CleanupTempFiles(time.Hour)

	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Errorf("aged temp file should be removed (stat err %v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh temp file should survive: %v", err)
	}
}

// captureLog redirects the std logger to buf for the duration of a test and
// returns a restore func. CleanupTempFiles only signals through the std
// logger, so "logged nothing" is the observable assertion. The package runs
// no t.Parallel tests, so the global redirect is safe.
func captureLog(t *testing.T, buf *bytes.Buffer) (restore func()) {
	t.Helper()
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}
