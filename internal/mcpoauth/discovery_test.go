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

// legacyASHandlers registers an RFC 8414 document at the ORIGIN root only (no
// PRM anywhere), the shape of an MCP server written against the 2025-03-26
// authorization flow.
func legacyASHandlers(mux *http.ServeMux, base string) {
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{
			Issuer:                        base + "/", // Cartesia spells it with a trailing slash
			AuthorizationEndpoint:         base + "/authorize",
			TokenEndpoint:                 base + "/token",
			RegistrationEndpoint:          base + "/register",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	})
}

// TestDiscoverLegacyOriginWhenNoPRM: no Protected Resource Metadata at any
// location (401 without a pointer, well-known forms 404) but the origin
// publishes authorization-server metadata — Intercom, Plaid, Cartesia,
// GoCardless, Square (#1006 audit). The origin becomes the AS, the typed URL
// the resource, and the result says so.
func TestDiscoverLegacyOriginWhenNoPRM(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth", error="invalid_token"`) // Intercom: 401, no resource_metadata
		w.WriteHeader(http.StatusUnauthorized)
	})
	legacyASHandlers(mux, base)

	d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !d.LegacyOrigin {
		t.Error("LegacyOrigin = false, want true")
	}
	if d.Resource != base+"/mcp" || d.PRM.Resource != base+"/mcp" {
		t.Errorf("resource = %q / prm %q, want the typed URL", d.Resource, d.PRM.Resource)
	}
	if len(d.PRM.AuthorizationServers) != 1 || d.PRM.AuthorizationServers[0] != base {
		t.Errorf("authorization_servers = %v, want [%s]", d.PRM.AuthorizationServers, base)
	}
	if d.AS.Issuer != base || d.AS.TokenEndpoint != base+"/token" || d.AS.RegistrationEndpoint != base+"/register" {
		t.Errorf("AS = %+v, want the origin's document with the issuer normalized to the origin", d.AS)
	}
}

// TestDiscoverNoPRMNoASNamesBothFailures: a server with neither document is
// still refused, and the error carries the PRM failure AND the legacy
// fallback's, so an operator sees the whole story.
func TestDiscoverNoPRMNoASNamesBothFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")
	if err == nil {
		t.Fatalf("Discover succeeded against a server with no metadata: %+v", d)
	}
	for _, want := range []string{"protected-resource metadata", "legacy MCP 2025-03-26 fallback", "authorization-server metadata"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err.Error(), want)
		}
	}
}

// TestDiscoverLegacyOriginStillVerifiesIssuer: the fallback extends no trust —
// an origin whose document claims another issuer is a mix-up and is rejected.
func TestDiscoverLegacyOriginStillVerifiesIssuer(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: "https://evil.example.com", AuthorizationEndpoint: "https://evil.example.com/a", TokenEndpoint: "https://evil.example.com/t"})
	})
	if _, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp"); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("legacy fallback accepted a foreign issuer: %v", err)
	}
}

// TestDiscoverPointerOnlyOnPOST: Uptime Robot's shape — GET on the MCP URL is
// 404 with no header, the 401 to a JSON-RPC initialize POST carries the
// resource_metadata pointer, and the document lives at the path-APPENDED
// well-known location that neither the inserted form nor the root serves.
func TestDiscoverPointerOnlyOnPOST(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	var methods []string
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/mcp/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/mcp/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: base + "/mcp", AuthorizationServers: []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", CodeChallengeMethodsSupported: []string{"S256"}})
	})
	d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.LegacyOrigin {
		t.Error("a served PRM must not be reported as the legacy fallback")
	}
	if strings.Join(methods, ",") != "GET,POST" {
		t.Errorf("server probed with %v, want GET then POST", methods)
	}
	if d.AS.TokenEndpoint != base+"/token" {
		t.Errorf("token endpoint = %q", d.AS.TokenEndpoint)
	}
}

// TestLocateResourceMetadataCandidatesOrder pins the well-known order when the
// server gives no pointer: inserted, appended, root — and root alone for a
// bare-origin URL.
func TestLocateResourceMetadataCandidatesOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	got, err := locateResourceMetadata(context.Background(), srv.Client(), srv.URL+"/v1/mcp")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		srv.URL + "/.well-known/oauth-protected-resource/v1/mcp",
		srv.URL + "/v1/mcp/.well-known/oauth-protected-resource",
		srv.URL + "/.well-known/oauth-protected-resource",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	got, _ = locateResourceMetadata(context.Background(), srv.Client(), srv.URL)
	if len(got) != 1 || got[0] != srv.URL+"/.well-known/oauth-protected-resource" {
		t.Errorf("bare-origin candidates = %v", got)
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

// TestVerifyAuthServerAcceptsEntraTemplatedIssuer: Microsoft Entra's
// multi-tenant metadata says issuer "https://login.microsoftonline.com/{tenantid}/v2.0"
// while the PRM named ".../organizations/v2.0" (measured against the hosted
// Azure DevOps MCP server for #1006). That one shape is accepted — and the
// stored issuer is the PRM's, not the template — while everything adjacent
// to it stays a mismatch.
func TestVerifyAuthServerAcceptsEntraTemplatedIssuer(t *testing.T) {
	templated := "https://login.microsoftonline.com/{tenantid}/v2.0"
	for _, expected := range []string{
		"https://login.microsoftonline.com/organizations/v2.0",
		"https://login.microsoftonline.com/common/v2.0",
		"https://login.microsoftonline.com/consumers/v2.0/",
		"https://LOGIN.microsoftonline.com/Organizations/v2.0",
	} {
		as := &AuthServerMetadata{Issuer: templated, AuthorizationEndpoint: "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize", TokenEndpoint: "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"}
		if err := verifyAuthServer(expected, as); err != nil {
			t.Errorf("expected %q: %v", expected, err)
		}
	}
	if err := verifyAuthServer("https://login.microsoftonline.us/organizations/v2.0", &AuthServerMetadata{Issuer: "https://login.microsoftonline.us/{tenantid}/v2.0"}); err != nil {
		t.Errorf("national cloud: %v", err)
	}
	rejected := map[string]string{
		// A tenant-specific expected issuer gets a tenant-specific document from Entra; a template there is wrong.
		"https://login.microsoftonline.com/72f988bf-86f1-41af-91ab-2d7cd011db47/v2.0": templated,
		// The template is Entra's; another host may not borrow the allowance.
		"https://evil.example.com/organizations/v2.0": "https://evil.example.com/{tenantid}/v2.0",
		// Same Entra host, but the document names a different host.
		"https://login.microsoftonline.com/organizations/v2.0": "https://evil.example.com/{tenantid}/v2.0",
		// Template in the wrong position / extra segments / v1 vs v2.
		"https://login.microsoftonline.com/organizations/v2.0/":  "https://login.microsoftonline.com/organizations/{tenantid}",
		"https://login.microsoftonline.com/organizations/v2.0//": "https://login.microsoftonline.com/{tenantid}/v2.0/extra",
		"https://login.microsoftonline.com/organizations/v1.0":   "https://login.microsoftonline.com/{tenantid}/v2.0",
		// http, never.
		"http://login.microsoftonline.com/organizations/v2.0": "http://login.microsoftonline.com/{tenantid}/v2.0",
	}
	for expected, actual := range rejected {
		if err := verifyAuthServer(expected, &AuthServerMetadata{Issuer: actual}); err == nil {
			t.Errorf("expected %q vs metadata %q: accepted, want mismatch", expected, actual)
		}
	}
	// The exact-match path is untouched by the allowance.
	if err := verifyAuthServer("https://as.example.com", &AuthServerMetadata{Issuer: "https://as.example.com/"}); err != nil {
		t.Errorf("exact match: %v", err)
	}
}

// TestRequestedScopesAddsOfflineAccessForEntraOnly: Entra mints a refresh
// token only when `offline_access` is requested, and the Azure DevOps PRM
// declares only its `.default` scope — so the scope is appended for an Entra
// issuer that advertises it, once, without touching the PRM slice, and for
// nobody else (#1006).
func TestRequestedScopesAddsOfflineAccessForEntraOnly(t *testing.T) {
	entra := &Discovered{
		PRM: ProtectedResourceMetadata{ScopesSupported: []string{"https://mcp.dev.azure.com/.default"}},
		AS: AuthServerMetadata{
			Issuer:          "https://login.microsoftonline.com/organizations/v2.0",
			ScopesSupported: []string{"openid", "profile", "email", "offline_access"},
		},
	}
	got := entra.RequestedScopes()
	want := []string{"https://mcp.dev.azure.com/.default", "offline_access"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("entra scopes = %v, want %v", got, want)
	}
	if entra.PRM.ScopesSupported[len(entra.PRM.ScopesSupported)-1] != "https://mcp.dev.azure.com/.default" {
		t.Error("RequestedScopes must not mutate the PRM slice")
	}
	// Already present (any case) → not duplicated.
	entra.PRM.ScopesSupported = []string{"https://mcp.dev.azure.com/.default", "Offline_Access"}
	if got := entra.RequestedScopes(); len(got) != 2 {
		t.Errorf("duplicate offline_access: %v", got)
	}
	// Entra that does not advertise offline_access → nothing appended.
	entra.PRM.ScopesSupported = []string{"x/.default"}
	entra.AS.ScopesSupported = []string{"openid"}
	if got := entra.RequestedScopes(); strings.Join(got, " ") != "x/.default" {
		t.Errorf("unadvertised: %v", got)
	}
	// Not Entra → PRM scopes verbatim, even when the AS lists offline_access.
	other := &Discovered{
		PRM: ProtectedResourceMetadata{ScopesSupported: []string{"read"}},
		AS:  AuthServerMetadata{Issuer: "https://as.example.com", ScopesSupported: []string{"read", "offline_access"}},
	}
	if got := other.RequestedScopes(); strings.Join(got, " ") != "read" {
		t.Errorf("non-entra: %v", got)
	}
	// No PRM scopes → the AS's list, as before.
	other.PRM.ScopesSupported = nil
	if got := other.RequestedScopes(); strings.Join(got, " ") != "read offline_access" {
		t.Errorf("as fallback: %v", got)
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
