package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	v, c, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d out of [43,128]", len(v))
	}
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if c != want {
		t.Errorf("challenge mismatch: got %q want %q", c, want)
	}
	// Two calls must differ.
	v2, _, _ := GeneratePKCE()
	if v == v2 {
		t.Error("two PKCE verifiers identical")
	}
}

func TestAuthCodeURL(t *testing.T) {
	f := FlowConfig{
		AuthorizationEndpoint: "https://as.example.com/authorize",
		ClientID:              "client123",
		RedirectURI:           "https://fleet.example.com/cb",
		Scopes:                []string{"mcp:read", "mcp:write"},
		Resource:              "https://mcp.example.com",
	}
	raw := f.AuthCodeURL("state-xyz", "challenge-abc")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "client123",
		"redirect_uri":          "https://fleet.example.com/cb",
		"scope":                 "mcp:read mcp:write",
		"state":                 "state-xyz",
		"code_challenge":        "challenge-abc",
		"code_challenge_method": "S256",
		"resource":              "https://mcp.example.com",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("auth URL param %q = %q, want %q", k, got, want)
		}
	}
	// Google's refresh-token parameters are Google's only: a server whose
	// issuer is anything else must not see them (prompt is an OIDC parameter
	// whose values a server may validate).
	for _, k := range []string{"access_type", "prompt"} {
		if q.Has(k) {
			t.Errorf("auth URL for a non-Google issuer carries %q=%q", k, q.Get(k))
		}
	}
}

// TestAuthCodeURLGoogleAsksForARefreshToken: Google issues a refresh token
// only when asked with access_type=offline, and only on the first consent
// unless prompt=consent forces the screen — without both, a Google Workspace
// connector lived one hour and had to be reconnected by hand (#1006). Both
// spellings of Google's issuer (the AS metadata has no trailing slash, the
// PRM has one) must select the parameters.
func TestAuthCodeURLGoogleAsksForARefreshToken(t *testing.T) {
	for _, issuer := range []string{"https://accounts.google.com", "https://accounts.google.com/"} {
		f := FlowConfig{
			AuthorizationEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
			ClientID:              "c",
			RedirectURI:           "https://fleet.example.com/cb",
			Issuer:                issuer,
		}
		u, err := url.Parse(f.AuthCodeURL("s", "ch"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := u.Query()
		if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
			t.Errorf("issuer %q: access_type=%q prompt=%q, want offline/consent", issuer, q.Get("access_type"), q.Get("prompt"))
		}
	}
	if isGoogleIssuer("https://accounts.google.com.evil.example") || isGoogleIssuer("https://evil.example/accounts.google.com") {
		t.Error("issuer matching must be on the exact host")
	}
}

// TestTokenRequest2xxErrorBodyIsOAuthError: GitHub's token endpoint answers a
// dead refresh token — and a wrong client secret — with HTTP 200 and an
// RFC 6749 §5.2 error body. Both must surface as the OAuthError they are, so
// the refresh path classifies them as terminal instead of retrying an opaque
// "no access_token" forever (#1006).
func TestTokenRequest2xxErrorBodyIsOAuthError(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body)) // HTTP 200, GitHub-style
	}))
	defer srv.Close()
	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", ClientSecret: "s"}

	body = `{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired."}`
	_, err := f.Refresh(context.Background(), srv.Client(), "ghr_dead")
	var oe *OAuthError
	if !errors.As(err, &oe) || oe.Code != "bad_refresh_token" || oe.HTTPStatus != http.StatusOK {
		t.Fatalf("refresh err = %v (%T), want OAuthError bad_refresh_token with HTTPStatus 200", err, err)
	}
	if !IsTerminalRefreshError(err) || !IsInvalidGrant(err) {
		t.Error("a dead GitHub refresh token must be terminal (and read as invalid_grant)")
	}
	if ReauthDetail(err) != "authorization expired — reconnect required" {
		t.Errorf("ReauthDetail = %q", ReauthDetail(err))
	}

	body = `{"error":"incorrect_client_credentials","error_description":"The client_id and/or client_secret passed are incorrect."}`
	_, err = f.Exchange(context.Background(), srv.Client(), "code", "verifier")
	if !errors.As(err, &oe) || oe.Code != "incorrect_client_credentials" {
		t.Fatalf("exchange err = %v, want OAuthError incorrect_client_credentials", err)
	}
	if !strings.Contains(ReauthDetail(err), "no longer recognizes this client") {
		t.Errorf("ReauthDetail = %q, want the client-credentials wording", ReauthDetail(err))
	}

	// A 200 with neither a token nor an error keeps the plain diagnostic.
	body = `{"token_type":"bearer"}`
	if _, err = f.Refresh(context.Background(), srv.Client(), "rt"); err == nil || errors.As(err, &oe) {
		t.Errorf("empty 200 body → %v, want a plain no-access_token error", err)
	}
}

func TestExchangeSendsResourceAndVerifier(t *testing.T) {
	var gotResource, gotVerifier, gotGrant string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotResource = r.Form.Get("resource")
		gotVerifier = r.Form.Get("code_verifier")
		gotGrant = r.Form.Get("grant_type")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", RedirectURI: "https://x/cb", Resource: "https://mcp.example.com"}
	tok, err := f.Exchange(context.Background(), srv.Client(), "code-1", "verifier-1")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Errorf("tokens = %+v", tok)
	}
	if tok.Expiry.IsZero() {
		t.Error("expiry not set from expires_in")
	}
	if gotResource != "https://mcp.example.com" {
		t.Errorf("resource = %q", gotResource)
	}
	if gotVerifier != "verifier-1" {
		t.Errorf("code_verifier = %q", gotVerifier)
	}
	if gotGrant != "authorization_code" {
		t.Errorf("grant_type = %q", gotGrant)
	}
}

func TestExchangeInvalidTargetFallback(t *testing.T) {
	// AS rejects the resource param the first time, succeeds without it.
	var sawResourceThenWithout []bool
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		hasResource := r.Form.Get("resource") != ""
		sawResourceThenWithout = append(sawResourceThenWithout, hasResource)
		if hasResource {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_target"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", RedirectURI: "https://x/cb", Resource: "https://mcp.example.com"}
	tok, err := f.Exchange(context.Background(), srv.Client(), "code", "verifier")
	if err != nil {
		t.Fatalf("Exchange with fallback: %v", err)
	}
	if tok.AccessToken != "at" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
	if len(sawResourceThenWithout) != 2 || !sawResourceThenWithout[0] || sawResourceThenWithout[1] {
		t.Errorf("expected [with-resource, without-resource], got %v", sawResourceThenWithout)
	}
}

func TestRefreshRotation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "old-rt" {
			t.Errorf("refresh_token = %q", r.Form.Get("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt", // rotation
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", Resource: "https://mcp.example.com"}
	tok, err := f.Refresh(context.Background(), srv.Client(), "old-rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "new-at" || tok.RefreshToken != "new-rt" {
		t.Errorf("rotated tokens = %+v", tok)
	}
}

func TestRefreshKeepsOldTokenWhenNotRotated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		// No refresh_token in the response — AS doesn't rotate.
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-at", "token_type": "Bearer", "expires_in": 60})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c"}
	tok, err := f.Refresh(context.Background(), srv.Client(), "keep-rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.RefreshToken != "keep-rt" {
		t.Errorf("expected old refresh token preserved, got %q", tok.RefreshToken)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "expired"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c"}
	_, err := f.Refresh(context.Background(), srv.Client(), "dead-rt")
	if !IsInvalidGrant(err) {
		t.Fatalf("expected invalid_grant, got %v", err)
	}
}

func TestClientSecretBasicAuth(t *testing.T) {
	mux := http.NewServeMux()
	var sawBasic bool
	var bodyHasSecret bool
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_, _, sawBasic = r.BasicAuth()
		_ = r.ParseForm()
		bodyHasSecret = r.Form.Get("client_secret") != ""
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{
		TokenEndpoint: srv.URL + "/token",
		ClientID:      "c", ClientSecret: "s",
		AuthMethods: []string{"client_secret_basic"},
	}
	if _, err := f.Refresh(context.Background(), srv.Client(), "rt"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sawBasic {
		t.Error("expected client_secret_basic (HTTP Basic) auth")
	}
	if bodyHasSecret {
		t.Error("client_secret should not be in the body when using Basic auth")
	}
}

// An AS may GRANT a narrower scope than fleet requested at connect time. fleet
// stores the requested scopes, so every subsequent refresh asks for too much and
// is refused with invalid_scope. RFC 6749 §6 makes `scope` optional on refresh
// and defines omitting it as "identical to the scope originally granted", so
// Refresh retries once without it. Without the retry the connection wedges
// permanently: the error is transient, so it repeats on every run forever.
func TestRefreshRetriesWithoutScopeOnInvalidScope(t *testing.T) {
	var attempts []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		attempts = append(attempts, r.Form.Get("scope"))
		if r.Form.Get("scope") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "invalid_scope", "error_description": "exceeds granted scope"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "narrow-at", "token_type": "Bearer", "expires_in": 60})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", Scopes: []string{"read", "write"}}
	tok, err := f.Refresh(context.Background(), srv.Client(), "rt")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.AccessToken != "narrow-at" {
		t.Errorf("access token = %q, want narrow-at", tok.AccessToken)
	}
	if len(attempts) != 2 || attempts[0] != "read write" || attempts[1] != "" {
		t.Errorf("attempts = %q, want [\"read write\", \"\"]", attempts)
	}
	// The refresh token is still preserved across the fallback path.
	if tok.RefreshToken != "rt" {
		t.Errorf("refresh token = %q, want rt preserved", tok.RefreshToken)
	}
}

// The scope fallback fires ONCE. A server that rejects the scopeless request too
// surfaces the error rather than looping.
func TestRefreshScopeFallbackDoesNotLoop(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_scope"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c", Scopes: []string{"read"}}
	if _, err := f.Refresh(context.Background(), srv.Client(), "rt"); !IsInvalidScope(err) {
		t.Fatalf("err = %v, want invalid_scope", err)
	}
	if calls != 2 {
		t.Errorf("token endpoint called %d times, want exactly 2 (one retry)", calls)
	}
}

// With no scopes configured there is nothing to drop, so invalid_scope surfaces
// on the first attempt instead of re-POSTing an identical request.
func TestRefreshNoScopeFallbackWhenNoScopesConfigured(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_scope"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := FlowConfig{TokenEndpoint: srv.URL + "/token", ClientID: "c"}
	if _, err := f.Refresh(context.Background(), srv.Client(), "rt"); !IsInvalidScope(err) {
		t.Fatalf("err = %v, want invalid_scope", err)
	}
	if calls != 1 {
		t.Errorf("token endpoint called %d times, want 1", calls)
	}
}

// The needs-reauth detail is shown in the connections UI, so it must name the
// real cause — and must never echo the authorization server's error_description,
// which is attacker-influenced free text from a user-supplied server.
func TestReauthDetailNamesCauseAndNeverEchoesServerText(t *testing.T) {
	const injected = "<script>alert(1)</script> contact evil.example"
	for _, tc := range []struct {
		code     string
		wantPart string
	}{
		{"invalid_client", "no longer recognizes this client"},
		{"unauthorized_client", "not permitted to refresh"},
		{"invalid_grant", "authorization expired"},
	} {
		got := ReauthDetail(&OAuthError{Code: tc.code, Description: injected, HTTPStatus: 400})
		if !strings.Contains(got, tc.wantPart) {
			t.Errorf("ReauthDetail(%q) = %q, want it to mention %q", tc.code, got, tc.wantPart)
		}
		if strings.Contains(got, "script") || strings.Contains(got, "evil.example") {
			t.Errorf("ReauthDetail(%q) echoed the server's error_description: %q", tc.code, got)
		}
	}
	if got := ReauthDetail(errors.New("boom")); got != "authorization expired — reconnect required" {
		t.Errorf("non-OAuth error detail = %q, want the generic fallback", got)
	}
}

func TestPublicClientAllowed(t *testing.T) {
	cases := []struct {
		name    string
		methods []string
		want    bool
	}{
		{"omitted list means client_secret_basic (RFC 8414 §2)", nil, false},
		{"basic only", []string{"client_secret_basic"}, false},
		{"post and basic", []string{"client_secret_post", "client_secret_basic"}, false},
		{"none listed", []string{"none"}, true},
		{"none among others, mixed case and padding", []string{"client_secret_basic", " None "}, true},
	}
	for _, tc := range cases {
		if got := PublicClientAllowed(tc.methods); got != tc.want {
			t.Errorf("%s: PublicClientAllowed(%v) = %v, want %v", tc.name, tc.methods, got, tc.want)
		}
	}
}
