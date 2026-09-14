package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// requireHTTPScheme refuses any URL that is not http:// or https:// before it
// reaches an outbound request. Remote-derived discovery URLs land here (see
// fetchJSON), and a file://, gopher:// or data:// pointer from a hostile server
// should be rejected by name rather than left to the transport to decline.
func requireHTTPScheme(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse discovery URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("refusing discovery URL with scheme %q (only http/https)", u.Scheme)
	}
}

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
	// Resource is the RFC 8707 resource indicator — the audience the issued
	// token is bound to: the PRM-declared `resource` when it shares the typed
	// server's origin, else the typed canonical URL. It is NOT necessarily the
	// MCP endpoint: Slack declares its bare origin while serving MCP at /mcp,
	// so callers keep the typed URL as the connection URL and send this value
	// only to the authorization server (#1006).
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
	prmURLs, err := locateResourceMetadata(ctx, httpClient, canonicalServerURL)
	if err != nil {
		return nil, err
	}

	var prm ProtectedResourceMetadata
	var prmURL string
	var fetchErr error
	for _, candidate := range prmURLs {
		var got ProtectedResourceMetadata
		if err := fetchJSON(ctx, httpClient, candidate, &got); err != nil {
			fetchErr = err
			continue
		}
		prm, prmURL = got, candidate
		break
	}
	if prmURL == "" {
		return nil, fmt.Errorf("fetch protected-resource metadata: %w", fetchErr)
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
	// Persist the issuer the PRM named and we verified against, not the
	// document's spelling of it: for an Entra multi-tenant endpoint the
	// document says the literal "{tenantid}" template (accepted by
	// issuerMatches), which is not a URL anyone can dial or key a vendor
	// clause on.
	if !strings.EqualFold(strings.TrimRight(issuer, "/"), strings.TrimRight(as.Issuer, "/")) {
		as.Issuer = issuer
	}

	return &Discovered{Resource: resource, PRM: prm, AS: *as}, nil
}

// RequestedScopes is the scope set the authorize request asks for: the
// PRM-declared `scopes_supported` (the resource knows what it needs), else the
// authorization server's.
//
// Microsoft Entra ID is the one vendor that needs a scope the resource never
// declares: it issues a refresh token only when `offline_access` is requested
// (MSAL appends it silently; a spec-driven client has to ask). Without it an
// Azure DevOps connector would live one hour and then sit in `needs_reauth`
// (the Google shape, #1006). `offline_access` is a standard OpenID Connect
// scope and Entra advertises it in `scopes_supported`, so it is appended only
// when the issuer is Entra and its metadata lists it — never sent to a
// vendor whose consent screen might reject an unknown scope.
func (d *Discovered) RequestedScopes() []string {
	scopes := d.PRM.ScopesSupported
	if len(scopes) == 0 {
		scopes = d.AS.ScopesSupported
	}
	out := append([]string(nil), scopes...)
	if isEntraIssuer(d.AS.Issuer) && containsFold(d.AS.ScopesSupported, "offline_access") && !containsFold(out, "offline_access") {
		out = append(out, "offline_access")
	}
	return out
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}

// locateResourceMetadata determines the Protected Resource Metadata URL
// candidates, in priority order. It first probes the server, hoping for a 401
// whose WWW-Authenticate header points at the metadata (RFC 9728 §5.1); failing
// that it falls back to the well-known locations: RFC 9728 §3.1's path-aware
// form first (/.well-known/oauth-protected-resource/<path> — what Google's
// Workspace MCP servers serve, and the MCP auth spec's prescribed location for
// a resource with a path component), then the origin-root form (what GitHub
// serves).
func locateResourceMetadata(ctx context.Context, httpClient *http.Client, canonicalServerURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canonicalServerURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes))
		if resp.StatusCode == http.StatusUnauthorized {
			if u := parseResourceMetadataURL(resp.Header.Get("WWW-Authenticate")); u != "" {
				return []string{u}, nil
			}
		}
	}
	// Fallback: the conventional well-known locations.
	origin, oerr := originOf(canonicalServerURL)
	if oerr != nil {
		return nil, oerr
	}
	root := origin + "/.well-known/oauth-protected-resource"
	if path := strings.TrimSuffix(strings.TrimPrefix(canonicalServerURL, origin), "/"); path != "" {
		return []string{root + path, root}, nil
	}
	return []string{root}, nil
}

// parseResourceMetadataURL pulls the resource_metadata parameter (RFC 9728
// §5.1) out of a WWW-Authenticate header. Returns "" when absent.
//
// The header is walked as the auth-params grammar of RFC 9110 §11.2 — scheme
// tokens, then comma-separated name=value pairs whose value is a token or a
// quoted-string with backslash escapes — rather than searched as a substring.
// The value this yields is a URL fleet will FETCH (a remote-derived URL, see
// the SSRF note on fetchJSON), so a substring match was two bugs at once: a
// param whose NAME merely ends in the key (x_resource_metadata=…) matched, and
// so did the text "resource_metadata=" sitting INSIDE another param's quoted
// value (error_description="… resource_metadata=https://attacker …"), where a
// quoted comma or escaped quote then also cut the value in the wrong place.
func parseResourceMetadataURL(header string) string {
	for _, p := range parseAuthParams(header) {
		if strings.EqualFold(p.name, "resource_metadata") {
			return strings.TrimSpace(p.value)
		}
	}
	return ""
}

// authParam is one name=value pair from a WWW-Authenticate header.
type authParam struct{ name, value string }

// parseAuthParams tokenizes a WWW-Authenticate header into its auth-params in
// order of appearance, across every challenge it carries. Scheme names and
// bare token68 values (a token not followed by "=") are skipped. It is lenient
// where leniency is harmless — an unquoted value runs to the next comma or
// whitespace even if it holds characters the token grammar forbids, since real
// servers emit resource_metadata=https://… unquoted — and strict where it
// matters: a quoted-string is one value however many commas, spaces or escaped
// quotes it holds, and a name matches only as a whole name.
func parseAuthParams(header string) []authParam {
	var params []authParam
	s := header
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			return params
		}
		// A name (or scheme) is a run of token characters.
		n := 0
		for n < len(s) && isTokenChar(s[n]) {
			n++
		}
		if n == 0 {
			// Not a token start (a stray quote or other punctuation): skip it.
			s = s[1:]
			continue
		}
		name := s[:n]
		rest := strings.TrimLeft(s[n:], " \t")
		if rest == "" || rest[0] != '=' {
			// A scheme ("Bearer") or a token68 credential: no value follows.
			s = rest
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		var value string
		if strings.HasPrefix(rest, `"`) {
			value, rest = readQuotedString(rest[1:])
		} else {
			end := strings.IndexAny(rest, ", \t")
			if end < 0 {
				end = len(rest)
			}
			value, rest = rest[:end], rest[end:]
		}
		params = append(params, authParam{name: name, value: value})
		s = rest
	}
}

// readQuotedString consumes the body of a quoted-string (the opening quote
// already removed) up to its closing quote, resolving backslash escapes, and
// returns the value plus the unconsumed remainder. An unterminated string runs
// to the end of the header.
func readQuotedString(s string) (value, rest string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), ""
}

// isTokenChar reports whether c may appear in an RFC 9110 token.
func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
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

// entraTenantTemplate is the literal placeholder Microsoft Entra ID puts in the
// `issuer` of the metadata served by its multi-tenant endpoints
// (login.microsoftonline.com/{common,organizations,consumers}/v2.0): the
// document says "https://login.microsoftonline.com/{tenantid}/v2.0" because
// the tenant is only known once a user signs in. RFC 8414 §3.3 wants the
// issuer to equal the URL the document was fetched from; Entra's does not, and
// every resource that names an Entra multi-tenant authorization server — the
// Microsoft-hosted Azure DevOps MCP server does — is otherwise unreachable
// (measured for #1006).
const entraTenantTemplate = "{tenantid}"

// issuerMatches is the mix-up-attack check: the metadata document must claim
// to be the issuer the PRM named. Exact (case-insensitive, trailing-slash
// tolerant) equality, plus one deliberately narrow allowance for Entra's
// templated issuer: same https host, an Entra host, same number of path
// segments, every segment equal except that the document may say
// entraTenantTemplate exactly where the expected issuer names one of Entra's
// multi-tenant aliases. Nothing else — a different host, a tenant GUID in the
// expected issuer, or the template anywhere else stays a mismatch.
func issuerMatches(expected, actual string) bool {
	e := strings.TrimRight(strings.TrimSpace(expected), "/")
	a := strings.TrimRight(strings.TrimSpace(actual), "/")
	if strings.EqualFold(e, a) {
		return true
	}
	if !isEntraIssuer(e) || !strings.Contains(strings.ToLower(a), entraTenantTemplate) {
		return false
	}
	eu, err := url.Parse(e)
	if err != nil {
		return false
	}
	au, err := url.Parse(a)
	if err != nil {
		return false
	}
	if eu.Scheme != "https" || au.Scheme != "https" || !strings.EqualFold(eu.Host, au.Host) || au.RawQuery != "" || au.Fragment != "" {
		return false
	}
	es := strings.Split(strings.Trim(eu.Path, "/"), "/")
	as := strings.Split(strings.Trim(au.Path, "/"), "/")
	if len(es) != len(as) {
		return false
	}
	templated := false
	for i := range es {
		if strings.EqualFold(as[i], entraTenantTemplate) {
			switch strings.ToLower(es[i]) {
			case "common", "organizations", "consumers":
				templated = true
				continue
			}
			return false
		}
		if !strings.EqualFold(es[i], as[i]) {
			return false
		}
	}
	return templated
}

// verifyAuthServer enforces the security-relevant invariants: the issuer must
// match (no mix-up via metadata from one issuer naming another; see
// issuerMatches for the one vendor allowance), and PKCE S256 must be supported
// (the MCP spec mandates it; an AS that only offers "plain" must be rejected,
// never silently downgraded).
func verifyAuthServer(expectedIssuer string, as *AuthServerMetadata) error {
	// `issuer` is REQUIRED by RFC 8414 §2 and is the anchor of the mix-up-attack
	// defense, so a document that omits it is rejected rather than waved through.
	if as.Issuer == "" {
		return fmt.Errorf("authorization-server metadata is missing the required issuer field")
	}
	if !issuerMatches(expectedIssuer, as.Issuer) {
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
//
// The URLs reaching here are REMOTE-DERIVED — a WWW-Authenticate
// `resource_metadata=` pointer, or a candidate built from a PRM-declared
// `issuer` — so they are untrusted even though the operator typed the server
// URL that led to them. SSRF is contained by SafeHTTPClient's resolve-then-dial
// guard and its no-redirect policy, and http.Transport would refuse a non-HTTP
// scheme anyway; the explicit check below is one line and makes that argument
// airtight rather than dependent on the transport's behavior.
func fetchJSON(ctx context.Context, httpClient *http.Client, url string, out any) error {
	if err := requireHTTPScheme(url); err != nil {
		return err
	}
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
