package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// When ADMIN_API_KEY is unset (h.config.AdminAPIKey == ""), verifyAdminKey must
// fail CLOSED. Otherwise sha256("") == sha256("") makes a request with NO
// X-API-Key header authenticate as admin, silently opening every admin route on
// a deployment that simply left the key unconfigured. Regression guard for the
// source-level fix that closes all six call sites at once.
func TestVerifyAdminKey_FailsClosedWhenUnconfigured(t *testing.T) {
	h := &Handlers{config: Config{AdminAPIKey: ""}}

	// No header at all — the exact empty/empty match that used to pass.
	if h.verifyAdminKey(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("verifyAdminKey returned true with no X-API-Key and no configured key — fail-open")
	}
	// An attacker guessing the empty string explicitly must also fail.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "")
	if h.verifyAdminKey(req) {
		t.Fatal("verifyAdminKey returned true for empty X-API-Key with no configured key — fail-open")
	}
}

func TestVerifyAdminKey_MatchesWhenConfigured(t *testing.T) {
	h := &Handlers{config: Config{AdminAPIKey: "s3cret"}}

	ok := httptest.NewRequest(http.MethodGet, "/", nil)
	ok.Header.Set("X-API-Key", "s3cret")
	if !h.verifyAdminKey(ok) {
		t.Fatal("verifyAdminKey rejected the correct configured key")
	}

	bad := httptest.NewRequest(http.MethodGet, "/", nil)
	bad.Header.Set("X-API-Key", "wrong")
	if h.verifyAdminKey(bad) {
		t.Fatal("verifyAdminKey accepted a wrong key")
	}
	// A missing header against a configured key must fail (not the empty match).
	if h.verifyAdminKey(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("verifyAdminKey accepted a missing header against a configured key")
	}
}

// The middleware that gates the admin-only routes must reject when no key is
// configured, exercising the fail-closed guard through the real entry point.
func TestAdminAuthMiddleware_RejectsWhenUnconfigured(t *testing.T) {
	h := &Handlers{config: Config{AdminAPIKey: ""}}
	reached := false
	handler := h.AdminAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/keys", nil))

	if reached {
		t.Fatal("AdminAuthMiddleware passed an unauthenticated request through with no configured key")
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("AdminAuthMiddleware returned 200 with no configured key; want a rejection, got %d", rec.Code)
	}
}
