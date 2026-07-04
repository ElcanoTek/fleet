package main

import (
	"context"
	"strings"
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

// TestPIIRedactorState: the three-hook state machine behind the PII settings.
// Load-bearing: engine switches rebuild the redactor, rampart without a URL
// is rejected AND rolls the state back (so a later valid change isn't
// poisoned), env rampart-without-URL degrades to pattern at the seed (never
// fail-open), and the probe reports the live engine.
func TestPIIRedactorState(t *testing.T) {
	t.Cleanup(func() { agentcore.SetPIIRedactor(nil) })

	st := newPIIRedactorState(&config.Config{})

	// Even while mode is off, engine=rampart without a URL is rejected — an
	// accepted-but-inert value would boobytrap the later mode change.
	if err := st.applyEngine("rampart", true); err == nil {
		t.Fatal("rampart without a URL must be rejected even in off mode")
	}

	if err := st.applyMode("redact", true); err != nil {
		t.Fatalf("mode: %v", err)
	}

	// rampart without a URL: rejected, and the engine field rolls back so the
	// state stays consistent for the next apply.
	if err := st.applyEngine("rampart", true); err == nil {
		t.Fatal("rampart without a URL must be rejected")
	}
	if st.engine != "pattern" {
		t.Fatalf("engine should roll back to pattern, got %q", st.engine)
	}

	// URL first, then engine: accepted; the installed redactor is rampart.
	if err := st.applyURL("http://127.0.0.1:1/v1/redact", true); err != nil {
		t.Fatalf("url: %v", err)
	}
	if err := st.applyEngine("rampart", true); err != nil {
		t.Fatalf("engine after url: %v", err)
	}
	if _, ok := st.current.(*piiredact.RampartRedactor); !ok {
		t.Fatalf("current redactor = %T, want RampartRedactor", st.current)
	}

	// Probe against the dead URL surfaces the failure (no silent fallback).
	res := st.Probe(context.Background())
	if res.OK || res.Engine != "rampart" {
		t.Errorf("probe against dead service = %+v, want ok=false engine=rampart", res)
	}

	// Off clears the redactor entirely.
	if err := st.applyMode("off", true); err != nil {
		t.Fatalf("off: %v", err)
	}
	if st.current != nil {
		t.Error("off must clear the current redactor")
	}

	// Env seed: rampart without a URL degrades to pattern (never fail-open).
	seeded := newPIIRedactorState(&config.Config{
		PIIRedactionEnabled: true, PIIRedactionMode: "redact", PIIRedactionEngine: "rampart",
	})
	if seeded.engine != "pattern" {
		t.Fatalf("env rampart without URL should seed as pattern, got %q", seeded.engine)
	}
	if err := seeded.rebuild(); err != nil {
		t.Fatalf("seeded rebuild: %v", err)
	}
	probe := seeded.Probe(context.Background())
	if !probe.OK || probe.Engine != "pattern" || probe.Redacted == "" {
		t.Errorf("pattern probe = %+v", probe)
	}
	if !strings.Contains(probe.Redacted, "[PII:email]") {
		t.Errorf("pattern probe should redact the synthetic sample: %q", probe.Redacted)
	}
}

// TestPIIRegistryOrderBootSafe replays what boot ApplyAll does — the real PII
// hooks, in ACTUAL registry order, with a persisted rampart config — and
// asserts the engine survives. Guards the url-before-engine registry ordering:
// with engine first, the hook would see the empty seed URL and reject a
// perfectly good persisted config at every boot (caught live before this test
// existed).
func TestPIIRegistryOrderBootSafe(t *testing.T) {
	t.Cleanup(func() { agentcore.SetPIIRedactor(nil) })

	st := newPIIRedactorState(&config.Config{})
	hooks := map[string]func(string, bool) error{
		"pii_redaction_mode":   st.applyMode,
		"pii_rampart_url":      st.applyURL,
		"pii_redaction_engine": st.applyEngine,
	}
	overrides := map[string]string{
		"pii_redaction_mode":   "redact",
		"pii_rampart_url":      "http://127.0.0.1:8787/v1/redact",
		"pii_redaction_engine": "rampart",
	}
	for _, spec := range settings.Registry() {
		hook, ok := hooks[spec.Key]
		if !ok {
			continue
		}
		if err := hook(overrides[spec.Key], true); err != nil {
			t.Fatalf("boot-order apply of %s failed: %v", spec.Key, err)
		}
	}
	if _, ok := st.current.(*piiredact.RampartRedactor); !ok {
		t.Fatalf("after boot-order apply, redactor = %T, want RampartRedactor", st.current)
	}
}
