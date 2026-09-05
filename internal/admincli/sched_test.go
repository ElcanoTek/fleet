package admincli

import (
	"bytes"
	"strings"
	"testing"
)

// TestEffectiveRateLimit — docs/FEATURE-NOTES.md (#722) promises that
// `--rate-limit` "remains a deprecated alias that warns" for
// --rate-limit-per-minute; the alias had gone missing, so scripts written
// against the documented flag failed with "flag provided but not defined".
// The alias fills in only when the primary is absent, warns on stderr (stdout
// stays clean for the shown-once secret), and passing both is refused.
func TestEffectiveRateLimit(t *testing.T) {
	parse := func(t *testing.T, argv ...string) (int, string, error) {
		t.Helper()
		fs, f := newAPIKeyCreateFlagSet()
		if err := fs.Parse(argv); err != nil {
			t.Fatalf("parse %q: %v", argv, err)
		}
		var warn bytes.Buffer
		n, err := effectiveRateLimit(fs, f, &warn)
		return n, warn.String(), err
	}

	n, warn, err := parse(t)
	if err != nil || n != 0 || warn != "" {
		t.Errorf("no flags: n=%d warn=%q err=%v", n, warn, err)
	}
	n, warn, err = parse(t, "--rate-limit-per-minute", "7")
	if err != nil || n != 7 || warn != "" {
		t.Errorf("primary: n=%d warn=%q err=%v", n, warn, err)
	}
	n, warn, err = parse(t, "--rate-limit", "5")
	if err != nil || n != 5 {
		t.Errorf("alias: n=%d err=%v", n, err)
	}
	if !strings.Contains(warn, "deprecated") || !strings.Contains(warn, "--rate-limit-per-minute 5") {
		t.Errorf("alias must warn and name the replacement: %q", warn)
	}
	if _, _, err := parse(t, "--rate-limit", "5", "--rate-limit-per-minute", "7"); err == nil {
		t.Error("both flags: want an error, got nil")
	}
	// The usage line documents the primary, and the alias is discoverable as
	// deprecated in the flag help.
	fs, _ := newAPIKeyCreateFlagSet()
	if fl := fs.Lookup("rate-limit"); fl == nil || !strings.Contains(fl.Usage, "DEPRECATED") {
		t.Errorf("--rate-limit should be defined and marked deprecated: %+v", fl)
	}
}
