package config

import "testing"

func TestStripQuotes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "double quotes", input: `"openai/gpt-5.2-chat"`, expected: "openai/gpt-5.2-chat"},
		{name: "single quotes", input: `'openai/gpt-5.2-chat'`, expected: "openai/gpt-5.2-chat"},
		{name: "no quotes", input: "openai/gpt-5.2-chat", expected: "openai/gpt-5.2-chat"},
		{name: "mismatched quotes", input: `"openai/gpt-5.2-chat'`, expected: `"openai/gpt-5.2-chat'`},
		{name: "empty string", input: "", expected: ""},
		{name: "only quotes", input: `""`, expected: ""},
		{name: "single quote char", input: `"`, expected: `"`},
		{name: "API key with quotes", input: `"sk-or-v1-abc123"`, expected: "sk-or-v1-abc123"},
		{name: "value with internal quotes", input: `"value with "internal" quotes"`, expected: `value with "internal" quotes`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripQuotes(tt.input)
			if result != tt.expected {
				t.Errorf("stripQuotes(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
