package redact

import (
	"strings"
	"testing"
)

// A literal that reads like logfmt syntax (`level=INFO`) is matched only
// inside one key or value, never across a field's `=` or the space between
// fields: ordinary text records keep their level field, while the literal
// inside a quoted value is still replaced (#2074 review: it rewrote every
// INFO header).
func TestScrubLineNeverMatchesLogfmtSyntax(t *testing.T) {
	r := NewRedactor(nil)
	r.AddLiteral("level=INFO")
	record := "time=2026-11-08T07:00:00Z level=INFO msg=ordinary n=1\n"
	if got := r.ScrubLine(record, false); got != record {
		t.Fatalf("the literal matched logfmt syntax: %q", got)
	}
	withValue := `time=t level=INFO msg="saw level=INFO in the body" k=v` + "\n"
	got := r.ScrubLine(withValue, false)
	if want := `time=t level=INFO msg="saw [REDACTED] in the body" k=v` + "\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !ValidLogfmt(got) {
		t.Fatalf("scrubbed line is not logfmt: %q", got)
	}
}

func TestValidLogfmt(t *testing.T) {
	for line, want := range map[string]bool{
		"a=1 b=\"x y\" c=\n":      true,
		`"odd key"=v g.k="a\"b"`:  true,
		"a=1 b=\"unterminated\n":  false,
		"just words\n":            false,
		`a="x"y`:                  false,
		"a=1 b=x\"y":              false,
		"msg=\"with = and \\\\\"": true,
	} {
		if got := ValidLogfmt(line); got != want {
			t.Errorf("ValidLogfmt(%q) = %v, want %v", line, got, want)
		}
	}
}

// A registered secret that is not inside a field (a line that is not
// logfmt) is still scrubbed: the parse never becomes a way around it.
func TestScrubLineScrubsANonLogfmtLineWhole(t *testing.T) {
	r := NewRedactor(nil)
	r.AddLiteral("opaqueSecret-77Hw")
	if got := r.ScrubLine("raw line opaqueSecret-77Hw\n", false); strings.Contains(got, "opaqueSecret-77Hw") {
		t.Fatalf("got %q", got)
	}
}
