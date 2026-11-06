package creds

import "testing"

// Moved from internal/config with the helpers they test (#1939): the env-file
// line parser and its quote/comment stripping now live only here.

func TestStripEnvInlineComment(t *testing.T) {
	cases := map[string]string{
		"60067 # Elcano default":   "60067",
		"50751\t# tab-then-hash":   "50751",
		"plain-value":              "plain-value",
		"value-with-#-mid":         "value-with-#-mid",
		`"60067 # quoted"`:         `"60067 # quoted"`,
		`'50751 # also quoted'`:    `'50751 # also quoted'`,
		"":                         "",
		"60067    # padded spaces": "60067",
	}
	for input, want := range cases {
		got := stripEnvInlineComment(input)
		if got != want {
			t.Errorf("stripEnvInlineComment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStripEnvQuotes(t *testing.T) {
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
			result := StripEnvQuotes(tt.input)
			if result != tt.expected {
				t.Errorf("StripEnvQuotes(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
