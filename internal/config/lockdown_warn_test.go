package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The "lockdown without an image" warning is a slog record, so a process
// that buffers its startup lines (fleet serve) replays it through the
// configured handler, format, level and FLEET_LOG_FILE — not a raw print to
// stderr (#2074 review).
func TestLoad_LockdownWithoutImageWarnsThroughSlog(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CHAT_LOCKDOWN_ONLY", "true")
	t.Setenv("CHAT_SANDBOX_IMAGE", "")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, "FLEET_LOCKDOWN_ONLY=true but the sandbox image is unset") {
		t.Fatalf("warning not logged through slog: %q", out)
	}
}
