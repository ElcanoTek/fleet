package config

import (
	"strings"
	"testing"
)

// TestEnvKnobBool covers the point-of-use bool resolver the outbound A2A
// peer registration reads (#1368): unset → default, a recognized token →
// its value, a malformed token → an error naming the knob.
func TestEnvKnobBool(t *testing.T) {
	const key = "FLEET_A2A_CLIENT_ALLOW_PRIVATE"
	t.Setenv(key, "")
	if v, err := EnvKnobBool(key, true); err != nil || !v {
		t.Fatalf("unset must return the default: %v %v", v, err)
	}
	for raw, want := range map[string]bool{"1": true, "yes": true, "on": true, "false": false, "0": false} {
		t.Setenv(key, raw)
		if v, err := EnvKnobBool(key, !want); err != nil || v != want {
			t.Errorf("%q → %v %v, want %v", raw, v, err, want)
		}
	}
	t.Setenv(key, "ture")
	if v, err := EnvKnobBool(key, false); err == nil || !strings.Contains(err.Error(), key) || v {
		t.Fatalf("malformed token must error naming the knob and keep the default: %v %v", v, err)
	}
	// A loader knob read through the external helper is a programming error.
	if _, err := EnvKnobBool("FLEET_A2A_ENABLED", false); err == nil || !strings.Contains(err.Error(), "BUG") {
		t.Fatalf("loader knob via EnvKnobBool must be reported as a bug: %v", err)
	}
	// The depth ceiling is registered as an external int with a floor of 1.
	t.Setenv("FLEET_A2A_MAX_DELEGATION_DEPTH", "0")
	if _, err := EnvKnobInt("FLEET_A2A_MAX_DELEGATION_DEPTH", DefaultA2AMaxDelegationDepth); err == nil {
		t.Fatal("a zero depth ceiling must be refused (min 1)")
	}
	t.Setenv("FLEET_A2A_MAX_DELEGATION_DEPTH", "5")
	if v, err := EnvKnobInt("FLEET_A2A_MAX_DELEGATION_DEPTH", DefaultA2AMaxDelegationDepth); err != nil || v != 5 {
		t.Fatalf("depth ceiling 5: %d %v", v, err)
	}
}
