package config

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestLoad_RetiredAccountEventsKnobsOnlyWarn: Fleet no longer publishes
// account events (ADR-0096). A leftover FLEET_ACCOUNT_EVENTS_* line, even one
// the old feed would have refused at boot (a URL with no secret), must not stop
// Fleet starting; Load names each one so the operator removes it.
func TestLoad_RetiredAccountEventsKnobsOnlyWarn(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "https://auth.example.com/apps/fleet/events")
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	if _, err := Load(""); err != nil {
		t.Fatalf("Load with a retired FLEET_ACCOUNT_EVENTS_URL: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "FLEET_ACCOUNT_EVENTS_URL is set but no longer used") || strings.Contains(out, "FLEET_ACCOUNT_EVENTS_SECRET is set") {
		t.Fatalf("boot log = %q, want a warning naming only FLEET_ACCOUNT_EVENTS_URL", out)
	}
	if strings.Contains(out, "auth.example.com") {
		t.Fatalf("the warning printed the URL: %q", out)
	}
}
