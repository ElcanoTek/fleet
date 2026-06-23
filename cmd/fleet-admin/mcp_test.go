package main

import "testing"

// TestServerMatchesVar pins the anchored matching that replaces the old
// unanchored substring scan: a short server token must match only whole
// underscore-delimited segments of a credential VAR, so it can no longer
// over-match (and, via del's ambiguity guard, silently destroy) an unrelated
// connector's credential.
func TestServerMatchesVar(t *testing.T) {
	cases := []struct {
		varOrKey string
		server   string
		want     bool
	}{
		{"FAST_IO_API_KEY", "io", true},      // whole segment
		{"RATIO_TOKEN", "io", false},         // substring of RATIO, not a segment — must NOT match
		{"FAST_IO_API_KEY_PROD", "io", true}, // trailing account segment doesn't break the match
		{"FAST_IO_API_KEY", "fast-io", true}, // hyphen normalized, multi-segment run
		{"FAST_IO_API_KEY", "fast_io", true}, // underscore form
		{"MAGNITE_API_KEY", "magnite", true}, // exact single segment
		{"MAGNITE_API_KEY", "magni", false},  // partial segment must NOT match
		{"XANDR_TOKEN", "", false},           // empty server never matches
	}
	for _, c := range cases {
		if got := serverMatchesVar(c.varOrKey, c.server); got != c.want {
			t.Errorf("serverMatchesVar(%q, %q) = %v, want %v", c.varOrKey, c.server, got, c.want)
		}
	}
}
