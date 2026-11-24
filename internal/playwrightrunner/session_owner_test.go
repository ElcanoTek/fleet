package playwrightrunner

import "testing"

// A live session opens only for its owner's account: the owner in other
// capitals is the owner, a look-alike of the address (KELVIN SIGN for k) is
// another account.
func TestAuthorizeMatchesOnlyTheOwnerAccount(t *testing.T) {
	t.Parallel()
	s := &LiveSession{ownerEmail: "kate@x.com", viewToken: "tok"}
	for _, c := range []struct {
		caller string
		want   bool
	}{
		{"kate@x.com", true},
		{" Kate@X.com ", true},
		{"Kate@x.com", false},
		{"other@x.com", false},
		{"", false},
	} {
		if got := s.Authorize(c.caller, "tok"); got != c.want {
			t.Errorf("Authorize(%q) = %v, want %v", c.caller, got, c.want)
		}
	}
}
