package clientconfig

import (
	"regexp"
	"testing"
)

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
		{name: "interior double hyphen", input: "a--b", expected: true},

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

		// Shape, not just charset: the name must open and close alphanumeric, so
		// these stay in step with store.userSkillNameShape, which rejects them.
		{name: "hyphen only", input: "-", expected: false},
		{name: "double hyphen only", input: "--", expected: false},
		{name: "leading hyphen", input: "-research", expected: false},
		{name: "trailing hyphen", input: "research-", expected: false},
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

// The doc comment on validSkillName claims it agrees with the regex
// store.userSkillNameShape applies to user-authored skills. A comment cannot
// enforce that, and the two drifted once already: the charset-only version
// accepted "-", "-foo" and "foo-", which the regex rejects, so a bundle could
// ship a skill name the skill builder would refuse to save. This walks every
// short string over an alphabet of the interesting character classes — where
// leading/trailing and empty-input bugs live — and fails if the two disagree.
//
// The regex is inlined rather than imported because it lives in internal/store,
// which must not become a dependency of clientconfig. Its length cap is the one
// deliberate divergence: validSkillName checks shape only and the caller reports
// length separately against maxSkillNameLen, so the corpus stays well under 64.
func TestValidSkillNameAgreesWithUserSkillShape(t *testing.T) {
	userSkillNameShape := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

	// One representative of each class: interior-legal letter and digit, the
	// hyphen, an uppercase letter, an underscore, and a multi-byte rune.
	alphabet := []string{"a", "0", "-", "A", "_", "é"}

	var corpus []string
	corpus = append(corpus, "")
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			s := prefix + c
			corpus = append(corpus, s)
			build(s, depth-1)
		}
	}
	build("", 3)

	for _, name := range corpus {
		if got, want := validSkillName(name), userSkillNameShape.MatchString(name); got != want {
			t.Errorf("validSkillName(%q) = %v, but userSkillNameShape says %v", name, got, want)
		}
	}
	t.Logf("compared %d names", len(corpus))
}
