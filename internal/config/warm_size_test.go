package config

import (
	"strings"
	"testing"
)

// TestLoad_SandboxWarmSizeZeroIsExpressible pins the #1264 semantics: unset
// means "derive" (the -1 sentinel), while an explicit 0 is a real value —
// no warm pool — and must survive Load distinguishable from unset. Before
// this, FLEET_SANDBOX_WARM_SIZE=0 was silently identical to unset and the
// derived 2..8 pool ran anyway.
func TestLoad_SandboxWarmSizeZeroIsExpressible(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.SandboxWarmSize != -1 {
		t.Fatalf("unset warm size = %d, want the -1 sentinel", cfg.SandboxWarmSize)
	}

	t.Setenv("FLEET_SANDBOX_WARM_SIZE", "0")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load warm size 0: %v", err)
	}
	if cfg.SandboxWarmSize != 0 {
		t.Fatalf("explicit 0 warm size = %d, want 0", cfg.SandboxWarmSize)
	}

	t.Setenv("FLEET_SANDBOX_WARM_SIZE", "5")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load warm size 5: %v", err)
	}
	if cfg.SandboxWarmSize != 5 {
		t.Fatalf("explicit warm size = %d, want 5", cfg.SandboxWarmSize)
	}
}

// TestLoad_SandboxWarmSizeRejectsNegative pins the #1299 seam: the knob
// registry's min bound refuses EVERY explicit negative — an explicit "-1" is a
// misconfiguration, not a spelling of "derive" (only unset means derive). The
// registry enforces this for boot, hot-reload, and `fleet validate-config`
// alike; nothing downstream should ever see Size < 0 and have to be
// incidentally safe about it.
func TestLoad_SandboxWarmSizeRejectsNegative(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())

	for _, v := range []string{"-1", "-2"} {
		t.Setenv("FLEET_SANDBOX_WARM_SIZE", v)
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "FLEET_SANDBOX_WARM_SIZE") {
			t.Fatalf("warm size %s: want a loud FLEET_SANDBOX_WARM_SIZE error, got %v", v, err)
		}
	}
}
