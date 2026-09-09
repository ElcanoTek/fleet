package main

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/piiredact"
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
	if _, _, err := buildWorkspaceSettings(cfg, nil); err != nil {
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

// TestPIIRedactorState: the mode hook behind the PII setting. Load-bearing: a
// non-off mode installs the deterministic pattern redactor (the only engine
// since ADR-0063), an invalid mode is rejected AND rolls the state back so a
// later valid change isn't poisoned, and "off" clears the redactor entirely.
func TestPIIRedactorState(t *testing.T) {
	t.Cleanup(func() { agentcore.SetPIIRedactor(nil) })

	st := newPIIRedactorState(&config.Config{})
	if st.mode != "off" || st.current != nil {
		t.Fatalf("default config should seed off with no redactor, got mode=%q current=%T", st.mode, st.current)
	}

	if err := st.applyMode("redact", true); err != nil {
		t.Fatalf("mode: %v", err)
	}
	pr, ok := st.current.(*piiredact.PatternRedactor)
	if !ok {
		t.Fatalf("current redactor = %T, want PatternRedactor", st.current)
	}
	if got := pr.Redact("mail alex.rivera@example.com").Text; got != "mail [PII:email]" {
		t.Errorf("installed redactor should redact the sample, got %q", got)
	}

	// An invalid mode is rejected and the previous mode survives.
	if err := st.applyMode("shred", true); err == nil {
		t.Fatal("invalid mode must be rejected")
	}
	if st.mode != "redact" || st.current == nil {
		t.Fatalf("failed apply must roll back: mode=%q current=%T", st.mode, st.current)
	}

	// Off clears the redactor entirely.
	if err := st.applyMode("off", true); err != nil {
		t.Fatalf("off: %v", err)
	}
	if st.current != nil {
		t.Error("off must clear the current redactor")
	}

	// Env seed: enabled + mode installs the pattern engine at boot.
	seeded := newPIIRedactorState(&config.Config{PIIRedactionEnabled: true, PIIRedactionMode: "observe"})
	if err := seeded.rebuild(); err != nil {
		t.Fatalf("seeded rebuild: %v", err)
	}
	if seeded.current == nil || seeded.current.Mode() != piiredact.ModeObserve {
		t.Errorf("seeded redactor = %T, want an observe-mode PatternRedactor", seeded.current)
	}
}
