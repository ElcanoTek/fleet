package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A shutdown that cut work short — chat turns force-cancelled, approved
// actions left executing with an unknown outcome — logs at Warn, so it shows
// at FLEET_LOG_LEVEL=warn; a clean drain is Info (#2052).
func TestShutdownDrainOutcomeLevels(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logChatDrain(t.Context(), true, time.Second, func() int { t.Fatal("cancel called on a clean drain"); return 0 })
	if out := buf.String(); !strings.Contains(out, `level=INFO msg="fleet: all in-flight chat turns drained within grace"`) {
		t.Fatalf("clean drain: %s", out)
	}
	buf.Reset()
	logChatDrain(t.Context(), false, time.Second, func() int { return 2 })
	if out := buf.String(); !strings.Contains(out, `level=WARN msg="fleet: grace period expired; force-cancelled in-flight chat turns"`) || !strings.Contains(out, "cancelled=2") {
		t.Fatalf("force-cancel not at Warn: %s", out)
	}
	buf.Reset()
	logApprovalDrain(t.Context(), false, time.Second)
	if out := buf.String(); !strings.Contains(out, "level=WARN") {
		t.Fatalf("approved actions left executing not at Warn: %s", out)
	}
	buf.Reset()
	logApprovalDrain(t.Context(), true, time.Second)
	if buf.Len() != 0 {
		t.Fatalf("drained approvals logged: %s", buf.String())
	}
}
