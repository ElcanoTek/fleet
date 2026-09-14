package mcpoauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// Percent-escapes in the issuer path are preserved verbatim: a decoded
	// %2F would become a path separator and a decoded %3F a query delimiter,
	// pointing every candidate at a URL the issuer never published.
	got = authServerMetadataCandidates("https://as.example.com/tenant%2Fone%3Fx/")
	want = []string{
		"https://as.example.com/.well-known/oauth-authorization-server/tenant%2Fone%3Fx",
		"https://as.example.com/.well-known/openid-configuration/tenant%2Fone%3Fx",
		"https://as.example.com/tenant%2Fone%3Fx/.well-known/openid-configuration",
		"https://as.example.com/tenant%2Fone%3Fx/.well-known/oauth-authorization-server",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("escaped-path candidates = %v, want %v", got, want)
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

// TestDiscoverSkipsCandidateThatFailsVerification: the inserted form is now
// asked first, so a vendor whose catch-all answers it with its ORIGIN-level
// document (issuer = origin, not the path — the Chargebee shape from the
// #1006 audit) or with a document lacking PKCE S256 must be skipped, not taken
// at its first word, and the valid appended document behind it used. Both the
// mix-up check and the PKCE check run per candidate; the error when every
// candidate fails names each rejection.
func TestDiscoverSkipsCandidateThatFailsVerification(t *testing.T) {
	for name, wrong := range map[string]func(base, issuer string) AuthServerMetadata{
		"issuer mismatch": func(base, _ string) AuthServerMetadata {
			return AuthServerMetadata{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", CodeChallengeMethodsSupported: []string{"S256"}}
		},
		"pkce plain only": func(base, issuer string) AuthServerMetadata {
			return AuthServerMetadata{Issuer: issuer, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", CodeChallengeMethodsSupported: []string{"plain"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			base := srv.URL
			issuer := base + "/mcp"
			var order []string
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
				w.WriteHeader(http.StatusUnauthorized)
			})
			mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: issuer, AuthorizationServers: []string{issuer}})
			})
			// The catch-all answers the two inserted forms with the wrong document.
			mux.HandleFunc("/.well-known/oauth-authorization-server/", func(w http.ResponseWriter, r *http.Request) {
				order = append(order, r.URL.Path)
				_ = json.NewEncoder(w).Encode(wrong(base, issuer))
			})
			mux.HandleFunc("/.well-known/openid-configuration/", func(w http.ResponseWriter, r *http.Request) {
				order = append(order, r.URL.Path)
				_ = json.NewEncoder(w).Encode(wrong(base, issuer))
			})
			// The appended OIDC form carries the valid, issuer-specific document.
			mux.HandleFunc("/mcp/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
				order = append(order, r.URL.Path)
				_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", CodeChallengeMethodsSupported: []string{"S256"}})
			})
			d, err := Discover(context.Background(), srv.Client(), issuer)
			if err != nil {
				t.Fatalf("Discover must skip the %s candidate and use the appended document: %v", name, err)
			}
			if d.AS.TokenEndpoint != issuer+"/token" {
				t.Errorf("token endpoint = %q, want the appended document's", d.AS.TokenEndpoint)
			}
			if len(order) != 3 || order[2] != "/mcp/.well-known/openid-configuration" {
				t.Errorf("request order = %v, want both inserted forms rejected then the appended OIDC form", order)
			}
		})
	}

	// Every candidate answers with a mismatched issuer: the error names each
	// rejection, so the operator sees the vendor's wrong document, not a 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: "https://elsewhere.example.com", AuthorizationEndpoint: "https://elsewhere.example.com/a", TokenEndpoint: "https://elsewhere.example.com/t"})
	}))
	t.Cleanup(srv.Close)
	_, err := fetchAuthServerMetadata(context.Background(), srv.Client(), srv.URL+"/tenant1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Count(err.Error(), "issuer mismatch") != 4 || !strings.Contains(err.Error(), "/.well-known/oauth-authorization-server/tenant1: ") {
		t.Errorf("error must name each rejected location with its reason: %q", err)
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

// TestProbeResourceMetadataPointerRefusesNonHTTP: the probe never dials a
// non-http(s) URL, whatever the caller passed.
func TestProbeResourceMetadataPointerRefusesNonHTTP(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { dialed = true; w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	for _, u := range []string{"file:///etc/passwd", "gopher://" + strings.TrimPrefix(srv.URL, "http://"), "ftp://example.com/mcp"} {
		if got, _ := probeResourceMetadataPointer(context.Background(), srv.Client(), u, http.MethodGet, ""); got != "" {
			t.Errorf("%s: got pointer %q, want none", u, got)
		}
	}
	if dialed {
		t.Error("a non-http(s) URL reached the network")
	}
}

// TestLocateResourceMetadataCandidatesOrder pins the well-known order when the
// server gives no pointer: inserted, appended, root — and root alone for a
// bare-origin URL.
func TestLocateResourceMetadataCandidatesOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	t.Cleanup(srv.Close)
	loc, err := locateResourceMetadata(context.Background(), srv.Client(), srv.URL+"/v1/mcp")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		srv.URL + "/.well-known/oauth-protected-resource/v1/mcp",
		srv.URL + "/v1/mcp/.well-known/oauth-protected-resource",
		srv.URL + "/.well-known/oauth-protected-resource",
	}
	got := loc.candidates
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	if loc.advertised || loc.probeErr != nil {
		t.Errorf("well-known guesses after a 404 must be neither advertised nor a probe failure: %+v", loc)
	}
	loc, _ = locateResourceMetadata(context.Background(), srv.Client(), srv.URL)
	got = loc.candidates
	if len(got) != 1 || got[0] != srv.URL+"/.well-known/oauth-protected-resource" {
		t.Errorf("bare-origin candidates = %v", got)
	}
	// A query string is part of the canonical URL but not of where the
	// document lives: every candidate is built from the path alone, and a
	// percent-escaped segment survives verbatim.
	loc, _ = locateResourceMetadata(context.Background(), srv.Client(), srv.URL+"/tenant%2Fone/mcp?tenant=x")
	got = loc.candidates
	want = []string{
		srv.URL + "/.well-known/oauth-protected-resource/tenant%2Fone/mcp",
		srv.URL + "/tenant%2Fone/mcp/.well-known/oauth-protected-resource",
		srv.URL + "/.well-known/oauth-protected-resource",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("query-scoped candidates = %v, want %v", got, want)
	}
}

// TestDiscoverLegacyOriginWhenPRMListsNoAuthorizationServers: RFC 9728 makes
// authorization_servers optional, and a document that omits it yields no
// authorization server any more than a missing document does — so the same
// backwards-compatibility fallback applies, issuer check included.
func TestDiscoverLegacyOriginWhenPRMListsNoAuthorizationServers(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		// no authorization_servers, but a same-origin resource and scopes the vendor DID publish
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: base + "/mcp/v1", ScopesSupported: []string{"read", "write"}})
	})
	legacyASHandlers(mux, base)
	d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !d.LegacyOrigin || d.AS.Issuer != base || len(d.PRM.AuthorizationServers) != 1 || d.PRM.AuthorizationServers[0] != base {
		t.Errorf("want the legacy-origin fallback, got %+v", d)
	}
	// The fetched document's other fields survive: its scopes drive
	// RequestedScopes and its same-origin resource is adopted as usual.
	if strings.Join(d.RequestedScopes(), " ") != "read write" {
		t.Errorf("RequestedScopes = %v, want the PRM's own scopes", d.RequestedScopes())
	}
	if d.Resource != base+"/mcp/v1" || d.PRM.Resource != base+"/mcp/v1" {
		t.Errorf("resource = %q / prm %q, want the PRM's same-origin resource adopted", d.Resource, d.PRM.Resource)
	}
	// ...and with no AS at the origin either, the error says the PRM named none.
	var srv2 *httptest.Server
	srv2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
			_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: srv2.URL + "/mcp"}) // a valid resource, no authorization_servers
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv2.Close)
	if _, err := Discover(context.Background(), srv2.Client(), srv2.URL+"/mcp"); err == nil || !strings.Contains(err.Error(), "lists no authorization_servers") || !strings.Contains(err.Error(), "legacy MCP 2025-03-26 fallback") {
		t.Errorf("error must carry both halves: %v", err)
	}
}

// TestDiscoverAdvertisedPointerFailureDoesNotFallBack: a modern server that
// advertises its PRM location on the 401 and then cannot serve it (5xx,
// malformed JSON, a timeout) is a server having a bad moment, not a
// 2025-03-26 server with no metadata. Even with valid authorization-server
// metadata at its origin, discovery must surface the failure rather than
// persist a synthesized configuration the server never published.
func TestDiscoverAdvertisedPointerFailureDoesNotFallBack(t *testing.T) {
	for name, serve := range map[string]func(w http.ResponseWriter){
		"500":            func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"malformed json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"resource": `)) },
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			base := srv.URL
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/prm"`)
				w.WriteHeader(http.StatusUnauthorized)
			})
			mux.HandleFunc("/prm", func(w http.ResponseWriter, _ *http.Request) { serve(w) })
			legacyASHandlers(mux, base) // the origin WOULD satisfy the legacy fallback
			d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
			if err == nil {
				t.Fatalf("Discover fell back to the origin behind an advertised pointer: %+v", d)
			}
			if !strings.Contains(err.Error(), "the server advertised at "+base+"/prm") {
				t.Errorf("error must name the advertised location: %v", err)
			}
		})
	}
}

// TestDiscoverWellKnownOperationalFailureDoesNotFallBack: the same rule for
// the guessed well-known locations — a 500, a 429 or malformed JSON at one of
// them is not "no metadata", so discovery surfaces it instead of taking the
// legacy-origin fallback, even when the origin would satisfy it. Only when
// every location answers 404/410 does the fallback apply.
func TestDiscoverWellKnownOperationalFailureDoesNotFallBack(t *testing.T) {
	for name, serve := range map[string]func(w http.ResponseWriter){
		"500":            func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"429":            func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) },
		"malformed json": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"resource": `)) },
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			base := srv.URL
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
			// The inserted form is failing; the appended and root forms are absent.
			mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) { serve(w) })
			legacyASHandlers(mux, base) // the origin WOULD satisfy the legacy fallback
			d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
			if err == nil {
				t.Fatalf("Discover fell back to the origin past an operational failure: %+v", d)
			}
			if !strings.Contains(err.Error(), "not a 404") || strings.Contains(err.Error(), "legacy MCP 2025-03-26 fallback") {
				t.Errorf("error must surface the operational failure and not mention the fallback: %v", err)
			}
		})
	}
	// 410 Gone is an absence too: the fallback still applies.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
	legacyASHandlers(mux, srv.URL)
	if d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp"); err != nil || !d.LegacyOrigin {
		t.Errorf("410 at every location must still take the legacy fallback: %v %+v", err, d)
	}
}

// TestProbeDoesNotDrainOpenEventStream: a server that accepts the
// unauthenticated initialize with a text/event-stream it then holds open must
// not stall the probe — only status and headers are read, the body is closed,
// and the session the server opened is still terminated.
func TestProbeDoesNotDrainOpenEventStream(t *testing.T) {
	var deletes []string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			deletes = append(deletes, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = io.Copy(io.Discard, r.Body) // so the server notices the client hanging up
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", "sess-sse")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select { // hold the stream open until the client goes away
			case <-r.Context().Done():
			case <-done:
			}
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	client := srv.Client() // no global timeout: a drain would hang here
	start := time.Now()
	got, err := probeResourceMetadataPointer(context.Background(), client, srv.URL+"/mcp", http.MethodPost, initializeProbeBody)
	if got != "" || err != nil {
		t.Errorf("probe = %q, %v; want no pointer and no error", got, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("probe took %s against an open event stream; it must not drain the body", d)
	}
	if strings.Join(deletes, ",") != "sess-sse" {
		t.Errorf("session termination DELETEs = %v, want exactly one for sess-sse", deletes)
	}
}

// TestDiscoverProbeTransportFailureDoesNotFallBack: the pointer may live only
// on the POST (Uptime Robot). If that request gets no answer at all, absent
// well-known documents prove nothing, and discovery must surface the failure
// rather than take the legacy-origin fallback, even when the origin would
// satisfy it.
func TestDiscoverProbeTransportFailureDoesNotFallBack(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	done := make(chan struct{})
	t.Cleanup(func() { close(done); srv.Close() })
	base := srv.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			select { // never answers: the client times out
			case <-r.Context().Done():
			case <-done:
			}
			return
		}
		http.NotFound(w, r)
	})
	legacyASHandlers(mux, base) // the origin WOULD satisfy the legacy fallback
	client := srv.Client()
	client.Timeout = 300 * time.Millisecond
	d, err := Discover(context.Background(), client, base+"/mcp")
	if err == nil {
		t.Fatalf("Discover fell back to the origin past a probe that never answered: %+v", d)
	}
	if !strings.Contains(err.Error(), "did not answer") || strings.Contains(err.Error(), "legacy MCP 2025-03-26 fallback") {
		t.Errorf("error must surface the probe failure and not mention the fallback: %v", err)
	}
}

// TestDiscoverPRMWithoutResourceIsMalformedNotLegacy: RFC 9728 §2 requires
// `resource`, and it is a URI. A document with no authorization_servers whose
// resource is absent or not a valid resource URI (`{}`, `{"resource":"x"}`
// all parse) is malformed, and must not be treated as "names no
// authorization server" and waved into the legacy-origin fallback.
func TestDiscoverPRMWithoutResourceIsMalformedNotLegacy(t *testing.T) {
	for name, doc := range map[string]string{
		"empty object":      `{}`,
		"scopes only":       `{"scopes_supported":["read"]}`,
		"relative resource": `{"resource":"x"}`,
		"ftp resource":      `{"resource":"ftp://example.com/mcp"}`,
		"userinfo resource": `{"resource":"https://user:pw@example.com/mcp"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(doc)) })
			legacyASHandlers(mux, srv.URL) // the origin WOULD satisfy the legacy fallback
			d, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")
			if err == nil {
				t.Fatalf("Discover accepted a PRM without resource via the legacy fallback: %+v", d)
			}
			if !strings.Contains(err.Error(), "required resource field is missing or not a valid URI") {
				t.Errorf("error must name the malformed document: %v", err)
			}
		})
	}
}

// TestDiscoverUnusablePRMAtFirstLocationTriesTheNext: a generic JSON
// catch-all at the inserted well-known location (`{}` parses) must not stop
// discovery when the appended location carries the real document — the same
// continue-past-it treatment malformed JSON already gets.
func TestDiscoverUnusablePRMAtFirstLocationTriesTheNext(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/mcp/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{Resource: base + "/mcp", AuthorizationServers: []string{base + "/as"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(AuthServerMetadata{Issuer: base + "/as", AuthorizationEndpoint: base + "/as/authorize", TokenEndpoint: base + "/as/token", CodeChallengeMethodsSupported: []string{"S256"}})
	})
	d, err := Discover(context.Background(), srv.Client(), base+"/mcp")
	if err != nil {
		t.Fatalf("Discover stopped at the unusable first location: %v", err)
	}
	if d.LegacyOrigin || d.AS.Issuer != base+"/as" {
		t.Errorf("want the appended location's real document, got %+v", d)
	}
}

// TestProbeTerminatesSessionItOpened: a server that accepts the unauthenticated
// initialize (200 + Mcp-Session-Id) has allocated a session the probe never
// wanted; the probe sends the Streamable HTTP DELETE for it and returns no
// pointer. A GET that happens to carry the header is not a session the probe
// opened and is left alone.
func TestProbeTerminatesSessionItOpened(t *testing.T) {
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			deletes = append(deletes, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Mcp-Session-Id", "sess-123")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	if got, _ := probeResourceMetadataPointer(context.Background(), srv.Client(), srv.URL+"/mcp", http.MethodPost, initializeProbeBody); got != "" {
		t.Errorf("a 200 must yield no pointer, got %q", got)
	}
	if strings.Join(deletes, ",") != "sess-123" {
		t.Errorf("session termination DELETEs = %v, want exactly one for sess-123", deletes)
	}
	deletes = nil
	_, _ = probeResourceMetadataPointer(context.Background(), srv.Client(), srv.URL+"/mcp", http.MethodGet, "")
	if len(deletes) != 0 {
		t.Errorf("a GET opened no session and must terminate none: %v", deletes)
	}
	if !strings.Contains(initializeProbeBody, `"protocolVersion":"`+ProbeProtocolVersion+`"`) {
		t.Errorf("probe body does not announce ProbeProtocolVersion: %s", initializeProbeBody)
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
