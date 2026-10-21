package usertoken

import "testing"

func TestFind(t *testing.T) {
	cases := []struct {
		in      string
		n       int  // spellings found
		inexact bool // FirstInexact reports one
		escaped bool // the first spelling is escaped
	}{
		{"", 0, false, false},
		{"plain text", 0, false, false},
		{"${FLEET_USER_EMAIL}", 1, false, false},
		{"To: ${FLEET_USER_EMAIL}, cc ${FLEET_USER_EMAIL}", 2, false, false},
		{"${OUTER:-${FLEET_USER_EMAIL}}", 1, false, false},
		// FLEET_USER_NAME is reserved with no token behind it: even its
		// canonical spelling is never exact.
		{"${FLEET_USER_NAME}", 1, true, false},
		{"Hi ${FLEET_USER_NAME}, <${FLEET_USER_EMAIL}>", 2, true, false},
		// Not a user token: identifier boundaries.
		{"${FLEET_USER_NAMES}", 0, false, false},
		{"${MY_FLEET_USER_EMAIL}", 0, false, false},
		{"${FLEET_USER_EMAIL_2}", 0, false, false},
		{"FLEET_USER_EMAIL", 0, false, false},
		// Escapes.
		{"$${FLEET_USER_EMAIL}", 1, true, true},
		{"x $${FLEET_USER_NAME} y", 1, true, true},
		{"$$${FLEET_USER_EMAIL}", 1, true, true},
		{"$${ FLEET_USER_EMAIL }", 1, true, true},
		{"$${\u00a0FLEET_USER_EMAIL\u00a0}", 1, true, true},
		// ASCII padding and colon forms.
		{"${ FLEET_USER_EMAIL }", 1, true, false},
		{"${\tFLEET_USER_NAME}", 1, true, false},
		{"${FLEET_USER_NAME:-team}", 1, true, false},
		{"${FLEET_USER_EMAIL:?who}", 1, true, false},
		// Unicode padding — RE2's \s does not match any of these.
		{"${\u00a0FLEET_USER_EMAIL\u00a0}", 1, true, false},
		{"${\u2003FLEET_USER_NAME}", 1, true, false},
		{"${FLEET_USER_EMAIL\u3000}", 1, true, false},
		{"${\u200bFLEET_USER_EMAIL}", 1, true, false},
		{"${\ufeffFLEET_USER_EMAIL}", 1, true, false},
		{"${FLEET_USER_EMAIL\u0085}", 1, true, false},
		// Mixed: the exact one first, then a bad one.
		{"${FLEET_USER_EMAIL} / $${FLEET_USER_NAME}", 2, true, false},
	}
	for _, tc := range cases {
		got := Find(tc.in)
		if len(got) != tc.n {
			t.Errorf("Find(%q) = %d spellings %+v, want %d", tc.in, len(got), got, tc.n)
			continue
		}
		if Mentions(tc.in) != (tc.n > 0) {
			t.Errorf("Mentions(%q) = %v, want %v", tc.in, tc.n == 0, tc.n > 0)
		}
		_, inexact := FirstInexact(tc.in)
		if inexact != tc.inexact {
			t.Errorf("FirstInexact(%q) = %v, want %v", tc.in, inexact, tc.inexact)
		}
		if tc.n > 0 && got[0].Escaped != tc.escaped {
			t.Errorf("Find(%q)[0].Escaped = %v, want %v", tc.in, got[0].Escaped, tc.escaped)
		}
	}
}
