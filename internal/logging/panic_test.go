package logging

import (
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// A recovered panic goes through the configured handler: logfmt under
// FLEET_LOG_FORMAT=text (not a JSON line mixed into the text stream), and
// into FLEET_LOG_FILE, not stderr only (#2074 review).
func TestRecoveredPanicUsesTheConfiguredFormatAndFile(t *testing.T) {
	restoreLogging(t)
	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "text", Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	func() {
		defer safe.Recover("unit.panic-format", nil)
		panic("boom")
	}()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, `level=ERROR msg="panic recovered"`) || !strings.Contains(out, "goroutine=unit.panic-format") {
		t.Fatalf("panic record not written as logfmt to the file sink:\n%s", out)
	}
	if strings.Contains(out, `{"time"`) {
		t.Fatalf("a JSON line is mixed into the text stream:\n%s", out)
	}
}
