package config

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0106: a removed CHAT_/CUTLASS_ spelling set in the process env or the
// env file is not applied, and boot says so once per spelling, naming the
// FLEET_ key and never the value.
func TestLoadWarnsAboutRemovedAliases(t *testing.T) {
	isolateEnv(t)
	unsetForTest(t, "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_TEMPERATURE", "FLEET_TEMPERATURE", "CHAT_RATE_PER_MIN", "CUTLASS_ALLOWED_DIRS")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "CHAT_MAX_COST_USD=7.5\nCHAT_RATE_PER_MIN=11\nCUTLASS_ALLOWED_DIRS=/srv/drops\n")
	t.Setenv("CUTLASS_TEMPERATURE", "0.9")

	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Temperature == 0.9 || cfg.MaxCostUSD == 7.5 {
		t.Errorf("a removed alias was applied: temperature %v, max cost %v", cfg.Temperature, cfg.MaxCostUSD)
	}
	if cfg.RatePerMinute != 11 {
		t.Errorf("CHAT_RATE_PER_MIN is a canonical setting, not a removed alias: got %d", cfg.RatePerMinute)
	}
	// CUTLASS_ALLOWED_DIRS is a canonical connector wire key (never an alias):
	// the env file still supplies it, so a bundle's ${CUTLASS_ALLOWED_DIRS}
	// keeps resolving.
	if got := os.Getenv("CUTLASS_ALLOWED_DIRS"); got != "/srv/drops" {
		t.Errorf("CUTLASS_ALLOWED_DIRS from the env file = %q, want /srv/drops", got)
	}
	out := buf.String()
	for _, want := range []string{
		"CHAT_MAX_COST_USD is set in the env file, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to FLEET_MAX_COST_USD",
		"CUTLASS_TEMPERATURE is set in the process env, but fleet no longer reads it (the CHAT_/CUTLASS_ aliases were removed, ADR-0106): rename it to FLEET_TEMPERATURE",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("want exactly one %q in the boot log:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CHAT_RATE_PER_MIN") || strings.Contains(out, "CUTLASS_ALLOWED_DIRS") || strings.Contains(out, "7.5") || strings.Contains(out, "0.9") {
		t.Errorf("boot log flagged a canonical setting or printed a value:\n%s", out)
	}
}
