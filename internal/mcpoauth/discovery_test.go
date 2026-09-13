package mcpoauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newDiscoveryServer stands up an MCP server + colocated authorization server.
// The MCP endpoint 401s with a WWW-Authenticate pointing at its PRM; the PRM
// names the same origin as the AS; the AS publishes RFC 8414 metadata.
func newDiscoveryServer(t *testing.T, withWWWAuth bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		if withWWWAuth {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
			Resource:             base + "/mcp",
			AuthorizationServers: []string{base},
			ScopesSupported:      []string{"mcp:read", "mcp:write"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{
			Issuer:                        base,
			AuthorizationEndpoint:         base + "/authorize",
			TokenEndpoint:                 base + "/token",
			RegistrationEndpoint:          base + "/register",
			ScopesSupported:               []string{"mcp:read", "mcp:write"},
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	return srv
}

func TestDiscoverViaWWWAuthenticate(t *testing.T) {
	srv := newDiscoveryServer(t, true)
	d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.AS.TokenEndpoint != srv.URL+"/token" {
		t.Errorf("token endpoint = %q", d.AS.TokenEndpoint)
	}
	if d.AS.RegistrationEndpoint != srv.URL+"/register" {
		t.Errorf("registration endpoint = %q", d.AS.RegistrationEndpoint)
	}
	if d.Resource != srv.URL+"/mcp" {
		t.Errorf("resource = %q, want %q", d.Resource, srv.URL+"/mcp")
	}
}

func TestDiscoverFallbackWellKnown(t *testing.T) {
	// No WWW-Authenticate header → must fall back to the well-known PRM path.
	srv := newDiscoveryServer(t, false)
	d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover (fallback): %v", err)
	}
	if d.AS.AuthorizationEndpoint != srv.URL+"/authorize" {
		t.Errorf("authorize endpoint = %q", d.AS.AuthorizationEndpoint)
	}
}

// Google's Workspace MCP servers (e.g. gmailmcp.googleapis.com/mcp/v1) serve
// their PRM ONLY at RFC 9728 §3.1's path-aware well-known location, return 200
// (not 401) to unauthenticated probes, and 404 the origin-root form. Discovery
// must find the path-inserted document.
func TestDiscoverFallbackPathAwareWellKnown(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL

	mux.HandleFunc("/mcp/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // stateless 200, no WWW-Authenticate hint
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp/v1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
			Resource:             base + "/mcp/v1",
			AuthorizationServers: []string{base},
		})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{
			Issuer:                        base,
			AuthorizationEndpoint:         base + "/authorize",
			TokenEndpoint:                 base + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})

	d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp/v1")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Resource != srv.URL+"/mcp/v1" {
		t.Errorf("resource = %q, want %q", d.Resource, srv.URL+"/mcp/v1")
	}
}

// TestAuthServerMetadataCandidatesOrder pins the MCP-spec order of well-known
// locations: an issuer without a path has the two origin forms; an issuer
// with a path tries RFC 8414's inserted form first, then OIDC inserted, then
// OIDC appended, and last the appended RFC 8414 form fleet historically asked
// for (#1006 catalog audit: 13 official vendors publish only the inserted
// form).
func TestAuthServerMetadataCandidatesOrder(t *testing.T) {
	got := authServerMetadataCandidates("https://as.example.com/")
	want := []string{
		"https://as.example.com/.well-known/oauth-authorization-server",
		"https://as.example.com/.well-known/openid-configuration",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("no-path candidates = %v, want %v", got, want)
	}
	got = authServerMetadataCandidates("https://access.stripe.com/mcp/")
	want = []string{
		"https://access.stripe.com/.well-known/oauth-authorization-server/mcp",
		"https://access.stripe.com/.well-known/openid-configuration/mcp",
		"https://access.stripe.com/mcp/.well-known/openid-configuration",
		"https://access.stripe.com/mcp/.well-known/oauth-authorization-server",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("path candidates = %v, want %v", got, want)
	}
	// A multi-segment path keeps every segment in place.
	got = authServerMetadataCandidates("https://mcp.datadoghq.com/v1/mcp")
	if got[0] != "https://mcp.datadoghq.com/.well-known/oauth-authorization-server/v1/mcp" || got[2] != "https://mcp.datadoghq.com/v1/mcp/.well-known/openid-configuration" {
		t.Errorf("multi-segment candidates = %v", got)
	}
}

// TestDiscoverAuthServerMetadataPathInserted: the issuer carries a path and
// the vendor publishes its metadata only where RFC 8414 §3.1 says — the
// well-known segment INSERTED between host and path — 404ing the appended
// forms fleet used to try. Discovery must succeed, and must ask for the
// inserted form first.
func TestDiscoverAuthServerMetadataPathInserted(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	issuer := base + "/tenant1"
	var order []string
	record := func(_ http.ResponseWriter, r *http.Request) bool {
		order = append(order, r.URL.Path)
		return strings.Contains(r.URL.Path, ".well-known/o")
	}
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: base + "/mcp", AuthorizationServers: []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/tenant1", func(w http.ResponseWriter, r *http.Request) {
		record(w, r)
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{
			Issuer:                        issuer,
			AuthorizationEndpoint:         issuer + "/authorize",
			TokenEndpoint:                 issuer + "/token",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if record(w, r) {
			http.NotFound(w, r) // every other well-known location 404s
			return
		}
		http.NotFound(w, r)
	})

	d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.AS.TokenEndpoint != issuer+"/token" {
		t.Errorf("token endpoint = %q", d.AS.TokenEndpoint)
	}
	if len(order) == 0 || order[0] != "/.well-known/oauth-authorization-server/tenant1" {
		t.Errorf("first metadata request = %v, want the RFC 8414 inserted form first", order)
	}
}

// TestDiscoverAuthServerMetadataPathAppendedStillWorks: a vendor that
// publishes only the OIDC path-APPENDED document (Entra's
// organizations/v2.0, GitHub before it moved) keeps working through the
// fallback order.
func TestDiscoverAuthServerMetadataPathAppendedStillWorks(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	issuer := base + "/org/v2.0"
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: base + "/mcp", AuthorizationServers: []string{issuer}})
	})
	mux.HandleFunc("/org/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token"})
	})
	if _, err := Discover(context.Background(), srv.Client(), base+"/mcp"); err != nil {
		t.Fatalf("Discover via appended OIDC form: %v", err)
	}
}

// TestFetchAuthServerMetadataErrorNamesEveryLocation: when every location
// 404s the error lists each one, not just the last — the audit's failures
// read as "openid-configuration: 404" alone and hid that the RFC 8414 form was
// never asked for.
func TestFetchAuthServerMetadataErrorNamesEveryLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	_, err := fetchAuthServerMetadata(context.Background(), srv.Client(), srv.URL+"/tenant1")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"/.well-known/oauth-authorization-server/tenant1",
		"/.well-known/openid-configuration/tenant1",
		"/tenant1/.well-known/openid-configuration",
		"/tenant1/.well-known/oauth-authorization-server",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err.Error(), want)
		}
	}
}

func TestDiscoverIgnoresCrossOriginPRMResource(t *testing.T) {
	// A PRM that names a resource on a DIFFERENT origin must NOT rebind the
	// stored identity — Discover keeps the requested server URL (RFC 9728 §3.3).
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
			Resource:             "https://evil.example.com/owned", // cross-origin
			AuthorizationServers: []string{base},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{
			Issuer: base, AuthorizationEndpoint: base + "/a", TokenEndpoint: base + "/t",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
	d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Resource != srv.URL+"/mcp" {
		t.Errorf("resource = %q, want the requested URL (cross-origin PRM resource must be ignored)", d.Resource)
	}
}

func TestVerifyAuthServerRejectsMissingIssuer(t *testing.T) {
	as := &AuthServerMetadata{
		AuthorizationEndpoint:         "https://as.example.com/a",
		TokenEndpoint:                 "https://as.example.com/t",
		CodeChallengeMethodsSupported: []string{"S256"},
		// Issuer deliberately empty — RFC 8414 makes it REQUIRED.
	}
	if err := verifyAuthServer("https://as.example.com", as); err == nil {
		t.Fatal("verifyAuthServer accepted metadata with no issuer")
	}
}

func TestVerifyAuthServerRejectsNonS256(t *testing.T) {
	as := &AuthServerMetadata{
		Issuer:                        "https://as.example.com",
		AuthorizationEndpoint:         "https://as.example.com/a",
		TokenEndpoint:                 "https://as.example.com/t",
		CodeChallengeMethodsSupported: []string{"plain"},
	}
	if err := verifyAuthServer("https://as.example.com", as); err == nil {
		t.Fatal("verifyAuthServer accepted an AS without S256")
	}
}

func TestVerifyAuthServerRejectsIssuerMismatch(t *testing.T) {
	as := &AuthServerMetadata{
		Issuer:                "https://evil.example.com",
		AuthorizationEndpoint: "https://evil.example.com/a",
		TokenEndpoint:         "https://evil.example.com/t",
	}
	if err := verifyAuthServer("https://as.example.com", as); err == nil {
		t.Fatal("verifyAuthServer accepted an issuer mismatch (mix-up attack)")
	}
}

func TestParseResourceMetadataURL(t *testing.T) {
	cases := map[string]string{
		`Bearer resource_metadata="https://x.com/.well-known/oauth-protected-resource"`: "https://x.com/.well-known/oauth-protected-resource",
		`Bearer realm="x", resource_metadata="https://y.com/rm", error="x"`:             "https://y.com/rm",
		`Bearer resource_metadata=https://z.com/rm, realm="x"`:                          "https://z.com/rm",
		`Bearer realm="x"`: "",
		``:                 "",
	}
	for in, want := range cases {
		if got := parseResourceMetadataURL(in); got != want {
			t.Errorf("parseResourceMetadataURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisterDCR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req clientRegistrationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.RedirectURIs) != 1 || req.RedirectURIs[0] != "https://fleet.example.com/cb" {
			t.Errorf("unexpected redirect_uris: %v", req.RedirectURIs)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ClientRegistration{ClientID: "abc123", RegistrationAccessToken: "rat"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg, err := Register(context.Background(), srv.Client(), srv.URL+"/register", "fleet", "https://fleet.example.com/cb", "mcp:read")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.ClientID != "abc123" {
		t.Errorf("client_id = %q", reg.ClientID)
	}
	if strings.TrimSpace(reg.RegistrationAccessToken) == "" {
		t.Error("expected registration access token")
	}

	// No registration endpoint → clear error.
	if _, err := Register(context.Background(), srv.Client(), "", "fleet", "x", ""); err == nil {
		t.Error("Register accepted an empty registration endpoint")
	}
}

// TestFetchJSONRefusesNonHTTPScheme pins the scheme guard on the remote-derived
// discovery URLs. A hostile MCP server controls the WWW-Authenticate
// `resource_metadata=` pointer and the PRM-declared `issuer`, so it chooses the
// string that reaches fetchJSON. SafeHTTPClient's dialer and http.Transport both
// already decline a non-HTTP scheme; this asserts we refuse it by name first, so
// the containment argument does not rest on transport behavior.
func TestFetchJSONRefusesNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:70/x",
		"data:application/json,{}",
		"ftp://example.com/meta.json",
	} {
		var out map[string]any
		err := fetchJSON(context.Background(), http.DefaultClient, raw, &out)
		if err == nil {
			t.Fatalf("fetchJSON(%q) = nil error, want refusal", raw)
		}
		if !strings.Contains(err.Error(), "only http/https") {
			t.Fatalf("fetchJSON(%q) error = %v, want a scheme refusal", raw, err)
		}
	}
}

// TestRequireHTTPSchemeAcceptsHTTPAndHTTPS is the negative half: the guard must
// not reject the two schemes discovery legitimately uses.
func TestRequireHTTPSchemeAcceptsHTTPAndHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:8080/.well-known/oauth-protected-resource",
		"https://example.com/.well-known/openid-configuration",
	} {
		if err := requireHTTPScheme(raw); err != nil {
			t.Fatalf("requireHTTPScheme(%q) = %v, want nil", raw, err)
		}
	}
}
