package agentcore

import (
	"strings"
	"testing"
)

// Admin-settings live overrides for the two per-turn agentcore knobs. The
// load-bearing assertions: an override wins over the env resolution, clearing
// reverts to it, and zero cannot disable the output boundary.

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
	if got := maxToolOutputBytes(); got != MinMaxToolOutputBytes {
		t.Errorf("small override: got %d want envelope floor %d", got, MinMaxToolOutputBytes)
	}
	// Zero is accepted for backward compatibility but now means the safe default,
	// never "no ceiling".
	SetMaxToolOutputBytes(0)
	if got := maxToolOutputBytes(); got != defaultMaxToolOutputBytes {
		t.Errorf("zero override: got %d want safe default %d", got, defaultMaxToolOutputBytes)
	}
	big := strings.Repeat("large prose ", defaultMaxToolOutputBytes)
	if out, truncated := applyOutputCeiling(big, 0); !truncated || len(out) > defaultMaxToolOutputBytes {
		t.Errorf("limit 0 must retain the safe boundary, truncated=%t bytes=%d", truncated, len(out))
	}
	SetMaxToolOutputBytes(HardMaxToolOutputBytes * 2)
	if got := maxToolOutputBytes(); got != HardMaxToolOutputBytes {
		t.Errorf("oversized override: got %d want hard cap %d", got, HardMaxToolOutputBytes)
	}

	// Negative clears the override back to the env resolution.
	SetMaxToolOutputBytes(-1)
	if got := maxToolOutputBytes(); got != base {
		t.Errorf("cleared: got %d want %d", got, base)
	}
}
