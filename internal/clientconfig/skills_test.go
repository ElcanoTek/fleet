package clientconfig

import "testing"

func TestValidSkillName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		// Valid cases
		{name: "lowercase letters", input: "research", expected: true},
		{name: "letters and hyphens", input: "research-report", expected: true},
		{name: "letters, numbers, and hyphens", input: "research-report-v2", expected: true},
		{name: "single letter", input: "r", expected: true},
		{name: "single number", input: "2", expected: true},
		{name: "hyphen only", input: "-", expected: true},

		// Invalid cases
		{name: "empty string", input: "", expected: false},
		{name: "uppercase letters", input: "Research", expected: false},
		{name: "spaces", input: "research report", expected: false},
		{name: "underscores", input: "research_report", expected: false},
		{name: "symbols", input: "research@report", expected: false},
		{name: "starts with uppercase", input: "Research-report", expected: false},
		{name: "ends with uppercase", input: "research-reporT", expected: false},
		{name: "contains uppercase", input: "research-Report", expected: false},
		{name: "newline", input: "research\nreport", expected: false},
		{name: "tab", input: "research\treport", expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := validSkillName(tc.input)
			if actual != tc.expected {
				t.Errorf("validSkillName(%q) = %v, expected %v", tc.input, actual, tc.expected)
			}
		})
	}
}
