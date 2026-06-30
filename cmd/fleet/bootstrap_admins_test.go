package main

import (
	"reflect"
	"testing"
)

// TestBootstrapAdmins covers the FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS parser
// (#458): it lowercases, trims, drops blanks, and de-duplicates while preserving
// first-seen order. Unset/empty yields no bootstrap (nil).
func TestBootstrapAdmins(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "unset", raw: "", want: nil},
		{name: "whitespace only", raw: "   ", want: nil},
		{name: "single", raw: "ops@example.com", want: []string{"ops@example.com"}},
		{name: "lowercases and trims", raw: "  Ops@Example.com ", want: []string{"ops@example.com"}},
		{
			name: "multiple with blanks",
			raw:  "a@x.com, ,b@x.com,",
			want: []string{"a@x.com", "b@x.com"},
		},
		{
			name: "dedupes case-insensitively, keeps first-seen order",
			raw:  "lead@x.com,A@x.com,a@x.com,LEAD@x.com",
			want: []string{"lead@x.com", "a@x.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS", tc.raw)
			got := bootstrapAdmins()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bootstrapAdmins(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}
