package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authn "github.com/ElcanoTek/fleet/internal/principal"
)

// CSRF and the rate-limit bucket choice key off the request principal's
// AuthMethod (#2050, ADR-0108), not off a header a caller can add.

func withPrincipal(r *http.Request, p authn.Principal) *http.Request {
	return r.WithContext(authn.With(r.Context(), p))
}

func TestCSRFByAuthMethod(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	reached := false
	mw := h.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	// Each case is a mutating request with NO Origin (what the gateway sends).
	cases := []struct {
		name   string
		method authn.AuthMethod
		want   int
	}{
		// The gateway enforced the browser's Origin before attaching these.
		{"session", authn.MethodSession, http.StatusOK},
		{"gateway", authn.MethodGateway, http.StatusOK},
		// A credential principal is only ever attached AFTER CSRF (by the auth
		// middleware), so on its own it exempts nothing: the credential's own
		// header (X-API-Key, Bearer) is what exempts such a request.
		{"api key principal without its header", authn.MethodAPIKey, http.StatusForbidden},
		{"no principal", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
			req.Host = "orchestrator.example.com"
			if tc.method != "" {
				req = withPrincipal(req, authn.Principal{Email: "u@x.com", AuthMethod: tc.method})
			}
			rr := httptest.NewRecorder()
			mw.ServeHTTP(rr, req)
			if rr.Code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Fatalf("code=%d reached=%v, want %d", rr.Code, reached, tc.want)
			}
		})
	}
	// An identity header never exempts anything.
	reached = false
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Host = "orchestrator.example.com"
	req.Header.Set("X-User-Email", "u@x.com")
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || reached {
		t.Fatalf("identity header alone bypassed CSRF: code=%d", rr.Code)
	}
}

// A request the gateway attached a principal to draws on the authenticated
// global bucket; an anonymous one never does.
func TestRateLimitBucketByAuthMethod(t *testing.T) {
	t.Parallel()
	h := New(Config{DefaultTaskModel: "test/model"}, nil, nil)
	for _, m := range []authn.AuthMethod{authn.MethodSession, authn.MethodGateway, authn.MethodAPIKey, authn.MethodBearer, authn.MethodAdminKey} {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/tasks", nil), authn.Principal{AuthMethod: m})
		if !h.rateLimitCallerAuthenticated(req) {
			t.Errorf("%s: not counted as authenticated", m)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Header.Set("X-User-Email", "u@x.com")
	if h.rateLimitCallerAuthenticated(req) {
		t.Error("an identity header alone counted as authenticated")
	}
}

// A gateway session's principal is the user, whatever identity headers ride
// along; a bare header is not an identity at all.
func TestUserPrincipalIgnoresIdentityHeaders(t *testing.T) {
	t.Parallel()
	h := newHeaderTrustHandler(t)
	var got string
	mw := h.AdminOrUserAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := GetUserFromContext(r.Context()); u != nil {
			got = u.Username
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := sessionRequest(http.MethodGet, "/tasks", "alice@elcanotek.com", authn.Session{})
	req.Header.Set("X-User-Email", "stranger@example.com")
	req.Header.Set("X-User-Session-Epoch", "forged")
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || got != "alice@elcanotek.com" {
		t.Fatalf("session + spoofed headers: code=%d user=%q", rr.Code, got)
	}

	got = ""
	req = httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set("X-User-Email", "alice@elcanotek.com")
	rr = httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || got != "" {
		t.Fatalf("bare identity header: code=%d user=%q, want 401", rr.Code, got)
	}

	// The resolved principal rides the context for the handlers.
	var p authn.Principal
	mw = h.AdminOrUserAuthMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, _ = authn.From(r.Context())
	}))
	mw.ServeHTTP(httptest.NewRecorder(), sessionRequest(http.MethodGet, "/tasks", "alice@elcanotek.com", authn.Session{}))
	if !p.Resolved() || p.UserID.String() == "00000000-0000-0000-0000-000000000000" || p.Role == "" {
		t.Fatalf("handler principal not resolved: %+v", p)
	}
}
