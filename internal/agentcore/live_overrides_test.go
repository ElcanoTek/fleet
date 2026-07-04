package agentcore

import "testing"

// Admin-settings live overrides for the two per-turn agentcore knobs. The
// load-bearing assertions: an override wins over the env resolution, clearing
// reverts to it, and 0 keeps its documented per-knob meaning ("unset" for the
// disclosure threshold, "no ceiling" for the output cap).

func TestSetToolDisclosureThresholdOverride(t *testing.T) {
	t.Cleanup(func() { SetToolDisclosureThreshold(0) })

	base := disclosureThreshold()
	if base != EnvToolDisclosureThreshold() {
		t.Fatalf("with no override, threshold %d should equal env resolution %d", base, EnvToolDisclosureThreshold())
	}

	SetToolDisclosureThreshold(7)
	if got := disclosureThreshold(); got != 7 {
		t.Errorf("override: got %d want 7", got)
	}

	// Clearing (<= 0) reverts to the env resolution.
	SetToolDisclosureThreshold(0)
	if got := disclosureThreshold(); got != base {
		t.Errorf("cleared: got %d want %d", got, base)
	}
	SetToolDisclosureThreshold(-3)
	if got := disclosureThreshold(); got != base {
		t.Errorf("negative clears: got %d want %d", got, base)
	}
}

func TestSetMaxToolOutputBytesOverride(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })

	base := maxToolOutputBytes() // whatever env/default resolved to in this process

	SetMaxToolOutputBytes(10)
	if got := maxToolOutputBytes(); got != 10 {
		t.Errorf("override: got %d want 10", got)
	}
	// 0 is a legal override meaning "no ceiling" — applyOutputCeiling treats it
	// as disabled — so it must NOT read as "unset".
	SetMaxToolOutputBytes(0)
	if got := maxToolOutputBytes(); got != 0 {
		t.Errorf("zero override: got %d want 0", got)
	}
	if out, truncated := applyOutputCeiling("some long content", 0); truncated || out != "some long content" {
		t.Errorf("limit 0 must disable truncation")
	}

	// Negative clears the override back to the env resolution.
	SetMaxToolOutputBytes(-1)
	if got := maxToolOutputBytes(); got != base {
		t.Errorf("cleared: got %d want %d", got, base)
	}
}
