package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
