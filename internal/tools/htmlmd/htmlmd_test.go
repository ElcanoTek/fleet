package htmlmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConvert_Goldens pins the conversion to testdata/*.md: each was generated
// from the matching .html by html-to-markdown v1.6.0 configured as web_fetch
// used it (NewConverter("", true, nil).ConvertString), before the library was
// removed. The corpus is the library's own test inputs plus fleet's edge
// cases (escaping, nested and malformed lists, inline spacing, links, code,
// blocks, Unicode, malformed HTML).
func TestConvert_Goldens(t *testing.T) {
	inputs, err := filepath.Glob("testdata/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) < 30 {
		t.Fatalf("only %d golden inputs", len(inputs))
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".html")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(strings.TrimSuffix(in, ".html") + ".md")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Convert(string(src))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output differs from the golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestEscapeMarkdown(t *testing.T) {
	for in, want := range map[string]string{
		"**bold**":       `\*\*bold\*\*`,
		"# h":            `\# h`,
		"1. x":           `1\. x`,
		"- x":            `\- x`,
		"> q":            `\> q`,
		"---":            `\-\-\-`,
		"[a](b)":         `\[a\](b)`,
		`a\*b`:           `a\\\*b`,
		"a|b `c` _d_":    "a\\|b \\`c\\` \\_d\\_",
		"plain sentence": "plain sentence",
	} {
		if got := escapeMarkdown(in); got != want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}
