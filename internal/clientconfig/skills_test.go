package clientconfig

import (
	"os"
	"path/filepath"
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

// TestDeclaredToolListShapes covers the allowed-tools shapes the roster parser
// has to survive. The list and comma-scalar forms are exercised in
// TestReadSkills; these are the ones that were not, and the mapping case is the
// one that mattered: it used to fail the whole frontmatter parse and drop the
// skill, over a field fleet surfaces for review and never enforces.
func TestDeclaredToolListShapes(t *testing.T) {
	dir := t.TempDir()
	writeSkill := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	front := func(name, allowed string) string {
		return "---\nname: " + name + "\ndescription: d\nallowed-tools: " + allowed + "\n---\nbody\n"
	}

	cases := []struct {
		name    string
		allowed string
		want    []string
	}{
		// Whitespace-separated scalar: named in the field's own doc comment,
		// previously untested.
		{"ws-scalar", `"Read Grep"`, []string{"Read", "Grep"}},
		// A block scalar reaches the same splitter via the newline separator.
		{"block-scalar", "|\n  Read\n  Grep\n", []string{"Read", "Grep"}},
		// Both empty forms mean "absent" — httpapi's omitempty depends on it.
		{"empty-list", "[]", nil},
		{"empty-scalar", `""`, nil},
		// An unrecognized shape must cost the FIELD, not the skill.
		{"mapping", "{Read: yes}", nil},
	}
	for _, tc := range cases {
		writeSkill(tc.name, front(tc.name, tc.allowed))
	}

	got, problems := ReadSkills(dir)
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	for _, tc := range cases {
		s, ok := byName[tc.name]
		if !ok {
			t.Errorf("%s: skill dropped from the roster (problems: %v)", tc.name, problems)
			continue
		}
		if len(s.DeclaredAllowedTools) != len(tc.want) {
			t.Errorf("%s: allowed-tools = %v, want %v", tc.name, s.DeclaredAllowedTools, tc.want)
			continue
		}
		for i, w := range tc.want {
			if s.DeclaredAllowedTools[i] != w {
				t.Errorf("%s: allowed-tools = %v, want %v", tc.name, s.DeclaredAllowedTools, tc.want)
				break
			}
		}
	}
	if len(problems) != 0 {
		t.Errorf("no case here is malformed enough to be a problem, got %v", problems)
	}
}
