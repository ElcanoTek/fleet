package admincli

import (
	"testing"
)

func TestStringSliceFlag_String(t *testing.T) {
	tests := []struct {
		name     string
		input    stringSliceFlag
		expected string
	}{
		{
			name:     "empty",
			input:    stringSliceFlag{},
			expected: "",
		},
		{
			name:     "single item",
			input:    stringSliceFlag{"a"},
			expected: "a",
		},
		{
			name:     "multiple items",
			input:    stringSliceFlag{"a", "b", "c"},
			expected: "a,b,c",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.input.String()
			if result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestStringSliceFlag_Set(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		initial  stringSliceFlag
		expected stringSliceFlag
	}{
		{
			name:     "empty string",
			input:    "",
			initial:  stringSliceFlag{},
			expected: stringSliceFlag{},
		},
		{
			name:     "whitespace string",
			input:    "   ",
			initial:  stringSliceFlag{},
			expected: stringSliceFlag{},
		},
		{
			name:     "single string",
			input:    "a",
			initial:  stringSliceFlag{},
			expected: stringSliceFlag{"a"},
		},
		{
			name:     "string with whitespace",
			input:    " a ",
			initial:  stringSliceFlag{},
			expected: stringSliceFlag{"a"},
		},
		{
			name:     "append to existing",
			input:    "b",
			initial:  stringSliceFlag{"a"},
			expected: stringSliceFlag{"a", "b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.initial
			err := s.Set(tc.input)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if len(s) != len(tc.expected) {
				t.Errorf("expected length %d, got %d", len(tc.expected), len(s))
			} else {
				for i := range s {
					if s[i] != tc.expected[i] {
						t.Errorf("expected %q at index %d, got %q", tc.expected[i], i, s[i])
					}
				}
			}
		})
	}
}
