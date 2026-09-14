package handlers

import (
	"testing"
)

// completed_status became a list so the dashboard's Failed Today card can ask
// for the two statuses its counter counts (error and dead_lettered). The parser
// is strict on purpose: a typo that matched nothing would render as "no
// failures today", which is precisely the false reassurance this filter exists
// to remove.
func TestParseCompletedStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"a single status, the pre-existing API shape", "success", []string{"success"}},
		{"the failure pair the card sends", "error,dead_lettered", []string{"error", "dead_lettered"}},
		{"whitespace a hand-written query picks up", " error , dead_lettered ", []string{"error", "dead_lettered"}},
		{"a trailing comma", "error,", []string{"error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCompletedStatuses(tc.raw)
			if err != nil {
				t.Fatalf("parse(%q): unexpected error %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parse(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parse(%q) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}

	for _, raw := range []string{"errored", "error,bogus", "'; DROP TABLE tasks; --", "running"} {
		t.Run("rejects "+raw, func(t *testing.T) {
			if _, err := parseCompletedStatuses(raw); err == nil {
				t.Errorf("parse(%q) accepted an unknown status; a silent empty result would "+
					"read to an operator as 'nothing failed today'", raw)
			}
		})
	}
}
