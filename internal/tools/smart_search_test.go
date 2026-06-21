package tools

import (
	"testing"
)

// TestIsDuckDuckGoBlocked is ported from cutlass; it exercises the
// shared block-page heuristic in the chat-base smart_search.go.
func TestIsDuckDuckGoBlocked(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		expected bool
	}{
		{
			name:     "Empty string",
			result:   "",
			expected: false,
		},
		{
			name:     "Short string",
			result:   "This is too short.",
			expected: true,
		},
		{
			name:     "No results found prefix",
			result:   "No results were found for the query you provided... and it's long enough.",
			expected: true,
		},
		{
			name:     "Long successful string",
			result:   "Here are the search results for your query. They contain multiple items, titles, URLs, and snippets. This text is definitely longer than fifty characters.",
			expected: false,
		},
		{
			name:     "Exact 50 chars but not blocked prefix",
			result:   "12345678901234567890123456789012345678901234567890",
			expected: false,
		},
		{
			name:     "Short string that would have panicked old code",
			result:   "No results",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDuckDuckGoBlocked(tt.result)
			if got != tt.expected {
				t.Errorf("isDuckDuckGoBlocked(%q) = %v; want %v", tt.result, got, tt.expected)
			}
		})
	}
}
