package main

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/settings"
)

// Boot-side wiring for the admin workspace settings. Load-bearing assertions:
// every registry key has a default and a hook (a new registry entry without
// wiring here must fail fast, not 500 at runtime), the PII default mirrors
// configurePIIRedaction's env semantics, and the live error-analysis gate
// short-circuits without touching the manager when disabled.

// TestBuildWorkspaceSettingsCoversRegistry: construction fails loudly on a
// wiring gap, so passing construction proves full coverage of the registry.
func TestBuildWorkspaceSettingsCoversRegistry(t *testing.T) {
	cfg := &config.Config{ErrorAnalysisEnabled: true, AutoTitle: true}
	if _, err := buildWorkspaceSettings(cfg, nil); err != nil {
		t.Fatalf("buildWorkspaceSettings should cover every registry key: %v", err)
	}
}

func TestDefaultPIIRedactionMode(t *testing.T) {
	cases := []struct {
		enabled bool
		mode    string
		want    string
	}{
		{false, "", "off"},
		{false, "block", "off"},  // master switch off wins
		{true, "", "redact"},     // enabled + unset → redact
		{true, "junk", "redact"}, // enabled + invalid → redact (control stays ON)
		{true, "off", "redact"},  // enabled + off is a contradiction → redact
		{true, "observe", "observe"},
		{true, "Block", "block"},
	}
	for _, c := range cases {
		cfg := &config.Config{PIIRedactionEnabled: c.enabled, PIIRedactionMode: c.mode}
		if got := defaultPIIRedactionMode(cfg); got != c.want {
			t.Errorf("enabled=%v mode=%q: got %q want %q", c.enabled, c.mode, got, c.want)
		}
	}
	// The derived default must always validate against the registry spec.
	for _, spec := range settings.Registry() {
		if spec.Key != "pii_redaction_mode" {
			continue
		}
		for _, c := range cases {
			cfg := &config.Config{PIIRedactionEnabled: c.enabled, PIIRedactionMode: c.mode}
			if _, err := settings.Validate(spec, defaultPIIRedactionMode(cfg)); err != nil {
				t.Errorf("derived default %q does not validate: %v", defaultPIIRedactionMode(cfg), err)
			}
		}
	}
}

// TestGatedErrorAnalyzerDisabled: the live gate returns (nil, nil) without
// touching the manager — a nil manager would panic if the gate leaked through.
func TestGatedErrorAnalyzerDisabled(t *testing.T) {
	cfg := &config.Config{ErrorAnalysisEnabled: false}
	g := gatedErrorAnalyzer{cfg: cfg, mgr: nil}
	analysis, err := g.AnalyzeTaskFailure(context.Background(), "p", "e", "t")
	if analysis != nil || err != nil {
		t.Fatalf("disabled gate: got (%v, %v), want (nil, nil)", analysis, err)
	}

	// Flipping the live setting re-arms the gate (nil manager now panics, which
	// is fine — production always wires a manager; errorAnalyzerFor returns nil
	// for a nil manager so the runner skips analysis entirely).
	if errorAnalyzerFor(cfg, nil) != nil {
		t.Fatal("nil manager must yield a nil analyzer seam")
	}
}
