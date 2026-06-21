package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The membership gate is the scoped-tier user-list check layered on top of
// authMiddleware. A validly-authenticated request (right shared token +
// X-User-Email) is admitted only when the email belongs to a provisioned
// chat user; everyone else gets 403 {"error":"not_a_member"} so the Next.js
// layer can render the no-access page instead of a login redirect loop.
//
// Unlike the handler-logic tests (which override Server.isMember to allow
// all), these point isMember back at the real store.IsUser and provision
// specific users so the gate is exercised end to end.

// memberFixture is a serverFixture wired to the REAL store.IsUser gate, with
// `members` provisioned. Any other email is a non-member.
func memberFixture(t *testing.T, members ...string) *Server {
	t.Helper()
	s := serverFixture(t)
	s.isMember = nil // fall back to store.IsUser
	for _, m := range members {
		seedUser(t, s.store, m)
	}
	return s
}

func TestMembership_KnownUserReaches200(t *testing.T) {
	s := memberFixture(t, "u@x.com")

	w := do(t, s.Routes(), http.MethodGet, "/auth/membership", nil, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("known user: status %d want 200 (body %q)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, `"member":true`) {
		t.Errorf("known user body = %q, want member:true", got)
	}
}

func TestMembership_UnknownUserRejected(t *testing.T) {
	s := memberFixture(t, "u@x.com") // stranger@x.com is deliberately not seeded

	w := do(t, s.Routes(), http.MethodGet, "/auth/membership", nil, "stranger@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown user: status %d want 403", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "not_a_member") {
		t.Errorf("unknown user body = %q, want it to mark not_a_member", got)
	}
}

func TestMembership_GatesDataEndpoints(t *testing.T) {
	s := memberFixture(t, "u@x.com")

	// An unknown but validly-authenticated user can't list conversations.
	w := do(t, s.Routes(), http.MethodGet, "/conversations", nil, "stranger@x.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown user /conversations: status %d want 403 (body %q)", w.Code, w.Body.String())
	}

	// A provisioned user reaches the handler (empty list, 200).
	w = do(t, s.Routes(), http.MethodGet, "/conversations", nil, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("known user /conversations: status %d want 200 (body %q)", w.Code, w.Body.String())
	}
}

// TestMembership_VerifyStaysOpen guards the enumeration property: the
// password pre-login endpoint /auth/verify must NOT be behind the membership
// gate, so an unknown email reaches the handler (and gets its generic
// "invalid credentials" answer) rather than a 403 that reveals non-membership.
func TestMembership_VerifyStaysOpen(t *testing.T) {
	s := memberFixture(t, "u@x.com")

	w := do(t, s.Routes(), http.MethodPost, "/auth/verify",
		map[string]string{"email": "stranger@x.com", "password": "whatever"}, "stranger@x.com")
	if w.Code == http.StatusForbidden {
		t.Fatalf("/auth/verify returned 403 for unknown email — membership gate must not wrap it (body %q)", w.Body.String())
	}
}
