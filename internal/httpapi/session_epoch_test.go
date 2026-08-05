package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Session revocation. The web session cookie is a stateless HMAC the Next.js
// tier verifies locally, so the users-table lookup membershipMiddleware already
// performs is the only place a "this session is no longer valid" decision can be
// made. The cookie carries the account's session epoch, the Next tier forwards
// it as X-User-Session-Epoch, and a mismatch means the password has changed since
// the cookie was minted.
//
// These run against the REAL store (memberFixture points isMember back at it),
// because the epoch comes from the same GetUser call that admits the user.

// getEpoch is `do` for a GET plus the forwarded session-epoch claim. An empty
// epoch sends no header at all — the elcano_auth case, whose cookie chat does
// not mint.
func getEpoch(t *testing.T, h http.Handler, path, user, epoch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", user)
	if epoch != "" {
		req.Header.Set("X-User-Session-Epoch", epoch)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// readEpoch drives GET /auth/session-epoch — the read both Next.js mint paths
// perform before stamping a cookie.
func readEpoch(t *testing.T, h http.Handler, user string) string {
	t.Helper()
	w := do(t, h, http.MethodGet, "/auth/session-epoch", nil, user)
	if w.Code != http.StatusOK {
		t.Fatalf("/auth/session-epoch for %s: status %d want 200 (body %q)", user, w.Code, w.Body.String())
	}
	var body struct {
		SessionEpoch string `json:"session_epoch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session epoch: %v", err)
	}
	if body.SessionEpoch == "" {
		t.Fatal("/auth/session-epoch returned an empty epoch")
	}
	return body.SessionEpoch
}

// The defect this exists for: an admin resets a compromised account's password
// and the attacker's stolen cookie keeps working for the rest of its 14 days.
func TestSessionEpoch_AdminPasswordResetEvictsOutstandingSessions(t *testing.T) {
	s := memberFixture(t, "boss@x.com", "u@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	stolen := readEpoch(t, h, "u@x.com")
	if w := getEpoch(t, h, "/conversations", "u@x.com", stolen); w.Code != http.StatusOK {
		t.Fatalf("pre-reset session: status %d want 200 (body %q)", w.Code, w.Body.String())
	}

	w := do(t, h, http.MethodPut, "/admin/users/u@x.com/password",
		map[string]string{"password": "brand-new-pw-1"}, "boss@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("password reset: status %d want 204 (body %q)", w.Code, w.Body.String())
	}

	w = getEpoch(t, h, "/conversations", "u@x.com", stolen)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("post-reset session: status %d want 401 (body %q)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, "session_revoked") {
		t.Errorf("post-reset body = %q, want it to mark session_revoked", got)
	}
	// The header is what the Next.js proxy funnel keys on to drop the stale
	// cookie; it must be readable without consuming the body.
	if got := w.Header().Get("X-Session-Revoked"); got != "1" {
		t.Errorf("X-Session-Revoked = %q, want \"1\"", got)
	}

	// A session minted after the reset works.
	fresh := readEpoch(t, h, "u@x.com")
	if fresh == stolen {
		t.Fatal("epoch unchanged across the reset")
	}
	if w := getEpoch(t, h, "/conversations", "u@x.com", fresh); w.Code != http.StatusOK {
		t.Fatalf("post-reset re-login: status %d want 200 (body %q)", w.Code, w.Body.String())
	}
}

// Every gated route is covered, not just the one above: the check lives in the
// middleware every one of them shares.
func TestSessionEpoch_StaleClaimRefusedOnEveryGatedRoute(t *testing.T) {
	s := memberFixture(t, "u@x.com")
	h := s.Routes()
	stolen := readEpoch(t, h, "u@x.com")
	if err := s.concreteStore(t).UpdatePassword(context.Background(), "u@x.com", "rotated-pw-99"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	for _, path := range []string{"/conversations", "/memories", "/auth/membership", "/personas"} {
		if w := getEpoch(t, h, path, "u@x.com", stolen); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d want 401 (body %q)", path, w.Code, w.Body.String())
		}
	}
}

func TestSessionEpoch_ForgedClaimRefused(t *testing.T) {
	s := memberFixture(t, "u@x.com")
	h := s.Routes()

	w := getEpoch(t, h, "/conversations", "u@x.com", "0000000000000000")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged claim: status %d want 401 (body %q)", w.Code, w.Body.String())
	}
}

// A request carrying no claim stays admitted: the Ed25519 elcano_auth cookie is
// minted by the auth service, which chat cannot add a claim to, so rejecting
// claimless requests would lock every magic-link user out.
func TestSessionEpoch_NoClaimStillAdmitted(t *testing.T) {
	s := memberFixture(t, "u@x.com")
	h := s.Routes()
	if err := s.concreteStore(t).UpdatePassword(context.Background(), "u@x.com", "rotated-pw-99"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	if w := getEpoch(t, h, "/conversations", "u@x.com", ""); w.Code != http.StatusOK {
		t.Fatalf("claimless request: status %d want 200 (body %q)", w.Code, w.Body.String())
	}
}

// Non-membership still outranks the epoch: a deleted account gets the 403 the
// no-access page keys on, not a revocation verdict that would say "log in again".
func TestSessionEpoch_NonMemberStillGetsNotAMember(t *testing.T) {
	s := memberFixture(t, "u@x.com")
	h := s.Routes()

	w := getEpoch(t, h, "/conversations", "stranger@x.com", "0000000000000000")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member: status %d want 403 (body %q)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, "not_a_member") {
		t.Errorf("non-member body = %q, want not_a_member", got)
	}
}

// /auth/session-epoch is on authMiddleware alone, like /auth/verify: the mint
// paths call it before a session exists, and an address chat has not provisioned
// must get an answer of the same shape rather than a 403 that would both break
// the login flow and reveal the user-list.
func TestSessionEpoch_EndpointAnswersForUnknownEmail(t *testing.T) {
	s := memberFixture(t, "u@x.com")
	h := s.Routes()

	stranger := readEpoch(t, h, "stranger@x.com")
	member := readEpoch(t, h, "u@x.com")
	if stranger == member {
		t.Error("an unprovisioned address reported the same epoch as a member")
	}
}
