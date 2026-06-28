package logging

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreStdLog saves and restores the process-global standard log destination
// so a test that flips it cannot leak into the others.
func restoreStdLog(t *testing.T) {
	t.Helper()
	w := log.Writer()
	flags := log.Flags()
	t.Cleanup(func() {
		log.SetOutput(w)
		log.SetFlags(flags)
	})
}

func TestConfigure_DisabledByDefaultPreservesStderr(t *testing.T) {
	restoreStdLog(t)

	// Point the standard log at a sentinel buffer; an off (empty-File) config must
	// leave it exactly there — i.e. it must NOT touch log.SetOutput at all, so the
	// default stderr/journald behaviour stays intact.
	var sentinel bytes.Buffer
	log.SetOutput(&sentinel)

	closer, err := Configure(Config{})
	if err != nil {
		t.Fatalf("Configure(off): %v", err)
	}
	if closer != nil {
		t.Fatalf("Configure(off) returned a non-nil closer; want nil (no sink installed)")
	}

	log.Print("hello")
	if !strings.Contains(sentinel.String(), "hello") {
		t.Errorf("off config redirected the standard log away from its existing writer: %q", sentinel.String())
	}
}

func TestConfig_Enabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("empty File: Enabled() should be false")
	}
	if !(Config{File: "/var/log/fleet/fleet.log"}).Enabled() {
		t.Error("non-empty File: Enabled() should be true")
	}
}

func TestConfigure_FileSinkTeesToFileAndStderr(t *testing.T) {
	restoreStdLog(t)

	// Redirect the process's real os.Stderr to a pipe so we can prove the sink
	// TEES rather than replacing: the line must reach BOTH the rotating file and
	// stderr (the journald-visible destination), so journald deployments keep
	// their lines even with a file sink on.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "fleet.log")
	closer, err := Configure(Config{File: logPath, MaxSizeMB: 1, MaxBackups: 1})
	if err != nil {
		t.Fatalf("Configure(file): %v", err)
	}
	if closer == nil {
		t.Fatal("Configure(file) returned a nil closer; want the rotating writer")
	}
	defer closer.Close()

	const marker = "rotation-sink-line"
	log.Print(marker)

	if err := closer.Close(); err != nil {
		t.Fatalf("close rotor: %v", err)
	}
	_ = w.Close()
	var stderrBuf bytes.Buffer
	if _, err := stderrBuf.ReadFrom(r); err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Errorf("log file missing the line; got %q", string(data))
	}
	// stderr must STILL receive the line — the sink tees, it does not steal.
	if !strings.Contains(stderrBuf.String(), marker) {
		t.Errorf("file sink did not also tee to stderr; got %q", stderrBuf.String())
	}
}

func TestConfigure_BadPathFailsLoudly(t *testing.T) {
	restoreStdLog(t)

	// A path whose parent is a regular file cannot be created; Configure must
	// surface that at startup rather than silently dropping every later line.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	badPath := filepath.Join(notADir, "fleet.log")

	closer, err := Configure(Config{File: badPath})
	if err == nil {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatalf("Configure(bad path) returned nil error; want a failure for %q", badPath)
	}
}
