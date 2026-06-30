package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxMetadataBytes caps a metadata/JSON response so a hostile server can't OOM
// the host by streaming an unbounded body.
const maxMetadataBytes = 1 << 20 // 1 MiB

// ProtectedResourceMetadata is the subset of RFC 9728 we use. It is published by
// the MCP (resource) server and points at the authorization server(s) that mint
// tokens for it.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// AuthServerMetadata is the subset of RFC 8414 (and the overlapping OIDC
// discovery document) we use to drive the authorization-code flow.
type AuthServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// Discovered bundles everything a caller needs to start an authorization flow.
type Discovered struct {
	// Resource is the canonical MCP server URI — the RFC 8707 resource indicator
	// and the audience the issued token is bound to.
	Resource string
	PRM      ProtectedResourceMetadata
	AS       AuthServerMetadata
}

// Discover walks the MCP authorization discovery chain for a canonical server
// URL: probe the server for a 401 + WWW-Authenticate pointer, fetch the RFC 9728
// Protected Resource Metadata, pick an authorization server, fetch its RFC 8414
// metadata, and verify it supports PKCE S256. httpClient MUST be the SSRF-safe
// client in production; tests inject a plain client against httptest.
func Discover(ctx context.Context, httpClient *http.Client, canonicalServerURL string) (*Discovered, error) {
	prmURL, err := locateResourceMetadata(ctx, httpClient, canonicalServerURL)
	if err != nil {
		return nil, err
	}

	var prm ProtectedResourceMetadata
	if err := fetchJSON(ctx, httpClient, prmURL, &prm); err != nil {
		return nil, fmt.Errorf("fetch protected-resource metadata: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("protected-resource metadata at %s lists no authorization_servers", prmURL)
	}

	// The canonical identity defaults to the URL the user typed. RFC 9728 §3.3
	// says the client SHOULD verify the PRM's `resource` matches the resource it
	// requested, so we only adopt the PRM-declared value when it shares the
	// requested server's ORIGIN — otherwise a server (or a same-origin attacker
	// who controls the PRM document) could silently rebind the stored identity /
	// connection URL to an arbitrary other resource. A non-matching value is
	// ignored (we keep the user's URL), not fatal.
	resource := canonicalServerURL
	if prm.Resource != "" {
		if c, cerr := CanonicalResourceURI(prm.Resource); cerr == nil && sameOrigin(c, canonicalServerURL) {
			resource = c
		}
	}

	issuer := strings.TrimSpace(prm.AuthorizationServers[0])
	as, err := fetchAuthServerMetadata(ctx, httpClient, issuer)
	if err != nil {
		return nil, err
	}

	if err := verifyAuthServer(issuer, as); err != nil {
		return nil, err
	}

	return &Discovered{Resource: resource, PRM: prm, AS: *as}, nil
}

// locateResourceMetadata determines the Protected Resource Metadata URL. It
// first probes the server, hoping for a 401 whose WWW-Authenticate header points
// at the metadata (RFC 9728 §5.1); failing that it falls back to the
// well-known location at the server's origin.
func locateResourceMetadata(ctx context.Context, httpClient *http.Client, canonicalServerURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canonicalServerURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes))
		if resp.StatusCode == http.StatusUnauthorized {
			if u := parseResourceMetadataURL(resp.Header.Get("WWW-Authenticate")); u != "" {
				return u, nil
			}
		}
	}
	// Fallback: the conventional well-known location at the origin.
	origin, oerr := originOf(canonicalServerURL)
	if oerr != nil {
		return "", oerr
	}
	return origin + "/.well-known/oauth-protected-resource", nil
}

// parseResourceMetadataURL pulls the resource_metadata="..." parameter out of a
// WWW-Authenticate: Bearer ... header. Returns "" when absent.
func parseResourceMetadataURL(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const key = "resource_metadata"
	idx := strings.Index(strings.ToLower(header), key+"=")
	if idx < 0 {
		return ""
	}
	v := header[idx+len(key)+1:]
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		v = v[1:]
		if end := strings.IndexByte(v, '"'); end >= 0 {
			v = v[:end]
		}
	} else {
		// Unquoted: ends at the next comma or whitespace.
		if end := strings.IndexAny(v, ", "); end >= 0 {
			v = v[:end]
		}
	}
	return strings.TrimSpace(v)
}

// fetchAuthServerMetadata tries the RFC 8414 well-known path and the OIDC
// discovery path against the issuer, returning the first that parses with a
// token_endpoint.
func fetchAuthServerMetadata(ctx context.Context, httpClient *http.Client, issuer string) (*AuthServerMetadata, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, fmt.Errorf("empty authorization server issuer")
	}
	candidates := []string{
		issuer + "/.well-known/oauth-authorization-server",
		issuer + "/.well-known/openid-configuration",
	}
	var lastErr error
	for _, c := range candidates {
		var as AuthServerMetadata
		if err := fetchJSON(ctx, httpClient, c, &as); err != nil {
			lastErr = err
			continue
		}
		if as.TokenEndpoint == "" || as.AuthorizationEndpoint == "" {
			lastErr = fmt.Errorf("authorization-server metadata at %s missing token/authorization endpoint", c)
			continue
		}
		return &as, nil
	}
	return nil, fmt.Errorf("fetch authorization-server metadata for %s: %w", issuer, lastErr)
}

// verifyAuthServer enforces the security-relevant invariants: the issuer must
// match (no mix-up via metadata from one issuer naming another), and PKCE S256
// must be supported (the MCP spec mandates it; an AS that only offers "plain"
// must be rejected, never silently downgraded).
func verifyAuthServer(expectedIssuer string, as *AuthServerMetadata) error {
	// `issuer` is REQUIRED by RFC 8414 §2 and is the anchor of the mix-up-attack
	// defense, so a document that omits it is rejected rather than waved through.
	if as.Issuer == "" {
		return fmt.Errorf("authorization-server metadata is missing the required issuer field")
	}
	if !strings.EqualFold(strings.TrimRight(expectedIssuer, "/"), strings.TrimRight(as.Issuer, "/")) {
		return fmt.Errorf("authorization-server issuer mismatch: metadata says %q, expected %q", as.Issuer, expectedIssuer)
	}
	// An empty methods list means the AS didn't advertise; the MCP spec requires
	// S256, so we proceed assuming S256. A non-empty list that omits S256 is a
	// hard reject.
	if len(as.CodeChallengeMethodsSupported) > 0 {
		ok := false
		for _, m := range as.CodeChallengeMethodsSupported {
			if strings.EqualFold(m, "S256") {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("authorization server does not support PKCE S256 (advertises %v)", as.CodeChallengeMethodsSupported)
		}
	}
	return nil
}

// fetchJSON GETs url and decodes a (size-limited) JSON body into out.
func fetchJSON(ctx context.Context, httpClient *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}
