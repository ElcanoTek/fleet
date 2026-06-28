package version

import (
	"regexp"
	"strings"
	"testing"
)

// TestVersionDefault: an unstamped build (the test binary is built without the
// release ldflag) reports the "dev" sentinel rather than a fabricated release
// number.
func TestVersionDefault(t *testing.T) {
	if got := Version(); got != "dev" {
		t.Errorf("Version() = %q, want %q (no ldflag stamped in tests)", got, "dev")
	}
}

// TestRevisionShape: Revision is either "unknown" or a short hex revision,
// optionally with a "+dirty" suffix, and is never longer than 12 hex digits plus
// that suffix. It must not panic regardless of build-info availability.
func TestRevisionShape(t *testing.T) {
	rev := Revision()
	if rev == "" {
		t.Fatal("Revision() = empty, want non-empty")
	}
	if rev == "unknown" {
		return
	}
	shape := regexp.MustCompile(`^[0-9a-f]{1,12}(\+dirty)?$`)
	if !shape.MatchString(rev) {
		t.Errorf("Revision() = %q, want short-hex or short-hex+dirty", rev)
	}
}

// TestString combines Version and Revision in the documented "<version> (<rev>)"
// shape.
func TestString(t *testing.T) {
	s := String()
	if !strings.HasPrefix(s, Version()+" (") || !strings.HasSuffix(s, ")") {
		t.Errorf("String() = %q, want %q-prefixed parenthesised revision", s, Version())
	}
	if !strings.Contains(s, Revision()) {
		t.Errorf("String() = %q, want it to contain Revision() %q", s, Revision())
	}
}
