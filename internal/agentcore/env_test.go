package agentcore

import (
	"os"
	"testing"
)

func TestEnvPrefix_lookupFloatDefault(t *testing.T) {
	prefix := EnvPrefix("TESTPREFIX")
	suffix := "TEST_FLOAT_DEFAULT_123"
	envKey := "TESTPREFIX_TEST_FLOAT_DEFAULT_123"
	defVal := 3.14

	tests := []struct {
		name     string
		setup    func(t *testing.T)
		expected float64
	}{
		{
			name: "unset returns default",
			setup: func(_ *testing.T) {
				// Explicitly unset to ensure it's not present
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
			if got := prefix.lookupFloatDefault(suffix, defVal); got != tt.expected {
				t.Errorf("lookupFloatDefault() = %v, want %v", got, tt.expected)
			}
		})
	}
}
