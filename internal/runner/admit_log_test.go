package runner

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Holding back scheduled claims is a degraded state (low disk, say): its
// transition is logged at Warn, so FLEET_LOG_LEVEL=warn still shows when it
// began; the recovery stays Info (#2074 review).
func TestHoldingBackClaimsIsLoggedAtWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	admit := false
	p := &Pool{admitScheduled: func() (bool, string) { return admit, "disk below the free-space floor" }}
	if p.admitClaims(context.Background()) {
		t.Fatal("claims admitted while the gate holds")
	}
	if out := buf.String(); !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, "holding back scheduled task claims") {
		t.Fatalf("hold not logged at warn: %q", out)
	}
}
