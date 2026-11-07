package agentcore

import (
	"os"
	"testing"
)

func TestFleetEnv_lookupFloatDefault(t *testing.T) {
	suffix := "CONTEXT_COMPACTION_THRESHOLD"
	envKey := "FLEET_CONTEXT_COMPACTION_THRESHOLD"
	defVal := 3.14

	tests := []struct {
		name     string
		setup    func(t *testing.T)
		expected float64
	}{
		{
			name: "unset returns default",
			setup: func(t *testing.T) {
				// Explicitly unset to ensure it's not present
				t.Setenv(envKey, "")
				os.Unsetenv(envKey)
			},
			expected: defVal,
		},
		{
			name: "valid float returns parsed value",
			setup: func(t *testing.T) {
				t.Setenv(envKey, "2.718")
			},
			expected: 2.718,
		},
		{
			name: "empty string returns default",
			setup: func(t *testing.T) {
				t.Setenv(envKey, "")
			},
			expected: defVal,
		},
		{
			name: "whitespace string returns default",
			setup: func(t *testing.T) {
				t.Setenv(envKey, "   ")
			},
			expected: defVal,
		},
		{
			name: "invalid string returns default",
			setup: func(t *testing.T) {
				t.Setenv(envKey, "not-a-number")
			},
			expected: defVal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			if got := fleetEnv.lookupFloatDefault(suffix, defVal); got != tt.expected {
				t.Errorf("lookupFloatDefault() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestFleetEnv_legacyAlias pins that a CHAT_/CUTLASS_ alias is still honored
// when the FLEET_ name is unset, and that FLEET_ wins when both are set.
func TestFleetEnv_legacyAlias(t *testing.T) {
	t.Setenv("FLEET_GUARDRAIL_PROFILE", "")
	t.Setenv("CUTLASS_GUARDRAIL_PROFILE", "legacy")
	if got := fleetEnv.lookup("GUARDRAIL_PROFILE"); got != "legacy" {
		t.Fatalf("lookup with only the CUTLASS_ alias set = %q, want %q", got, "legacy")
	}
	t.Setenv("FLEET_GUARDRAIL_PROFILE", "canonical")
	if got := fleetEnv.lookup("GUARDRAIL_PROFILE"); got != "canonical" {
		t.Fatalf("lookup with both set = %q, want the FLEET_ value", got)
	}
}
