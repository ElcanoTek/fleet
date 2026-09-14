package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	// MFAChallengeEndpoint is Auth0's proprietary metadata field. It is the
	// reliable marker of an Auth0 tenant behind a custom domain (Checkly's
	// auth.checklyhq.com publishes it), and Auth0 has one rule the generic
	// flow needs to know: a refresh token is issued only when `offline_access`
	// is requested. See RequestedScopes.
	MFAChallengeEndpoint string `json:"mfa_challenge_endpoint"`
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
	// LegacyOrigin is true when the authorization server was taken to be the
	// MCP server's own origin, as the MCP spec's backwards-compatibility rule
	// for 2025-03-26 servers directs. That happens in two shapes: the server
	// published no Protected Resource Metadata at all, in which case PRM is
	// synthesized (resource = the typed URL, authorization_servers = [origin])
	// and nothing in it came from the vendor; or it published a document with
	// a valid resource but no authorization_servers, in which case PRM is the
	// vendor's document with only authorization_servers filled in as [origin]
	// — its resource and scopes are real. Either way AS.Issuer is the origin.
	LegacyOrigin bool
}

// Discover walks the MCP authorization discovery chain for a canonical server
// URL: probe the server for a 401 + WWW-Authenticate pointer, fetch the RFC 9728
// Protected Resource Metadata, pick an authorization server, fetch its RFC 8414
// metadata, and verify it supports PKCE S256. httpClient MUST be the SSRF-safe
// client in production; tests inject a plain client against httptest.
func Discover(ctx context.Context, httpClient *http.Client, canonicalServerURL string) (*Discovered, error) {
	loc, err := locateResourceMetadata(ctx, httpClient, canonicalServerURL)
	if err != nil {
		return nil, err
	}

	var prm ProtectedResourceMetadata
	var prmURL string
	var fetchErr, operationalErr error
	for _, candidate := range loc.candidates {
		var got ProtectedResourceMetadata
		if err := fetchJSON(ctx, httpClient, candidate, &got); err != nil {
			fetchErr = err
			if !metadataAbsent(err) {
				operationalErr = err
			}
			continue
		}
		// A document that parses but is unusable — no authorization server
		// AND no usable `resource` (`{}` and `{"resource":"x"}` both parse)
		// — is malformed, not absent: a generic JSON catch-all
		// at one well-known location must not stop the appended or root
		// location from being tried, and must never be waved into the
		// legacy-origin fallback as "names no authorization server".
		if err := usablePRM(candidate, &got); err != nil {
			fetchErr, operationalErr = err, err
			continue
		}
		prm, prmURL = got, candidate
		break
	}
	if prmURL == "" {
		// The legacy-origin fallback is for a server that HAS no Protected
		// Resource Metadata — every location answered 404/410. A server that
		// told us where its metadata is (RFC 9728 §5.1) and then could not
		// serve it, or a well-known location that answered 5xx, timed out or
		// returned malformed JSON, is a modern server having a bad moment;
		// falling back to its origin would persist a synthesized
		// configuration the server never published. Surface that failure so
		// the operator retries.
		if loc.advertised {
			return nil, fmt.Errorf("fetch protected-resource metadata the server advertised at %s: %w", loc.candidates[0], fetchErr)
		}
		if operationalErr != nil {
			return nil, fmt.Errorf("fetch protected-resource metadata: %w (not a 404, so the server is not treated as one without metadata; retry, or check the server)", operationalErr)
		}
		if loc.probeErr != nil {
			// A probe that got no answer at all (timeout, reset) may have
			// been the one request the server puts its pointer on — Uptime
			// Robot names it only on the POST. Absent well-known documents
			// prove nothing then; the server did not get to speak.
			return nil, fmt.Errorf("probe the MCP server for its protected-resource metadata pointer: %w (the server did not answer, so it is not treated as one without metadata; retry, or check the server)", loc.probeErr)
		}
		return discoverLegacyOrigin(ctx, httpClient, canonicalServerURL, fetchErr)
	}

	var as *AuthServerMetadata
	legacy := false
	if len(prm.AuthorizationServers) == 0 {
		// RFC 9728 §2 makes authorization_servers OPTIONAL. A document that
		// omits it (usablePRM has already required a valid `resource`)
		// yields no authorization server any more than a missing document
		// does, so the same backwards-compatibility rule applies: the MCP
		// server's own origin is the authorization server, still subject to
		// the issuer check. The document's OTHER fields (scopes_supported,
		// resource) are the vendor's word and are kept.
		origin, las, lerr := legacyOriginAuthServer(ctx, httpClient, canonicalServerURL,
			fmt.Errorf("protected-resource metadata at %s lists no authorization_servers", prmURL))
		if lerr != nil {
			return nil, lerr
		}
		prm.AuthorizationServers = []string{origin}
		as, legacy = las, true
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

	if as == nil {
		issuer := strings.TrimSpace(prm.AuthorizationServers[0])
		// fetchAuthServerMetadata returns only a document that passed
		// verifyAuthServer for this issuer, or a proxied copy that
		// confirmProxiedIssuer accepted — re-checking the issuer here would
		// refuse the second kind.
		as, err = fetchAuthServerMetadata(ctx, httpClient, issuer)
		if err != nil {
			return nil, err
		}
		// Persist the issuer the PRM named and we verified against, not the
		// document's spelling of it: for an Entra multi-tenant endpoint the
		// document says the literal "{tenantid}" template (accepted by
		// issuerMatches), which is not a URL anyone can dial or key a vendor
		// clause on.
		// A confirmed proxied issuer (confirmProxiedIssuer) is the real
		// authorization server and stays as the document's own spelling.
		if issuerMatches(issuer, as.Issuer) && !strings.EqualFold(strings.TrimRight(issuer, "/"), strings.TrimRight(as.Issuer, "/")) {
			as.Issuer = issuer
		}
	}

	return &Discovered{Resource: resource, PRM: prm, AS: *as, LegacyOrigin: legacy}, nil
}

// usablePRM reports whether a fetched Protected Resource Metadata document can
// drive discovery: it names at least one authorization server, or — for the
// legacy-origin rule — it carries the `resource` RFC 9728 §2 requires, in a
// form fleet can canonicalize (CanonicalResourceURI: absolute, http(s), a
// host, no userinfo — the same validator every stored resource passes). That
// is deliberately fleet's own bar, not the RFC's full one: the canonicalizer
// accepts http and drops a fragment so that development and test servers
// work, and this check inherits exactly that. A document with neither is
// malformed, and the error says so by location.
func usablePRM(location string, prm *ProtectedResourceMetadata) error {
	if len(prm.AuthorizationServers) > 0 {
		return nil
	}
	if _, cerr := CanonicalResourceURI(prm.Resource); cerr != nil {
		return fmt.Errorf("protected-resource metadata at %s names no authorization_servers and its required resource field is missing or not a resource URI fleet can use (%w): a malformed document, not a server without metadata", location, cerr)
	}
	return nil
}

// discoverLegacyOrigin is the MCP spec's backwards-compatibility rule for
// servers that predate RFC 9728 Protected Resource Metadata (the 2025-03-26
// authorization flow): when no PRM can be fetched, the MCP server's own origin
// IS the authorization server, so its RFC 8414 / OIDC document is looked for
// there and the resource is the URL the user typed. Intercom, Plaid, Cartesia,
// GoCardless and Square still publish only this shape (#1006 catalog audit,
// 2026-09-13) and fleet refused all five with "fetch protected-resource
// metadata". A PRM that exists but names no authorization server takes the
// same path. The origin is the same party the PRM would have named, so no new
// trust is extended; the issuer check still applies to what it publishes.
// prmErr is why the PRM yielded nothing, kept in the error when the fallback
// fails too so an operator sees both halves of why the server could not be
// added.
func discoverLegacyOrigin(ctx context.Context, httpClient *http.Client, canonicalServerURL string, prmErr error) (*Discovered, error) {
	origin, as, err := legacyOriginAuthServer(ctx, httpClient, canonicalServerURL, prmErr)
	if err != nil {
		return nil, err
	}
	return &Discovered{
		Resource:     canonicalServerURL,
		PRM:          ProtectedResourceMetadata{Resource: canonicalServerURL, AuthorizationServers: []string{origin}},
		AS:           *as,
		LegacyOrigin: true,
	}, nil
}

// legacyOriginAuthServer fetches and verifies the authorization-server metadata
// at the MCP server's own origin — the 2025-03-26 rule shared by "no PRM at
// all" and "a PRM that names no authorization server". The verified issuer IS
// the origin; it is stored in that exact spelling (Cartesia's document says it
// with a trailing slash) so the row keys on one form. prmErr is why the PRM
// yielded nothing and is carried into the error when the origin has nothing
// either.
func legacyOriginAuthServer(ctx context.Context, httpClient *http.Client, canonicalServerURL string, prmErr error) (string, *AuthServerMetadata, error) {
	origin, oerr := originOf(canonicalServerURL)
	if oerr != nil {
		return "", nil, fmt.Errorf("fetch protected-resource metadata: %w", prmErr)
	}
	as, err := fetchAuthServerMetadata(ctx, httpClient, origin)
	if err != nil {
		return "", nil, fmt.Errorf("fetch protected-resource metadata: %w; and the server origin %s publishes no authorization-server metadata either (legacy MCP 2025-03-26 fallback): %w", prmErr, origin, err)
	}
	if err := verifyAuthServer(origin, as); err != nil {
		return "", nil, fmt.Errorf("legacy authorization server at the MCP origin: %w", err)
	}
	as.Issuer = origin
	return origin, as, nil
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
	// Vendors whose refresh-token contract needs `offline_access` asked for
	// explicitly, and who advertise it: Microsoft Entra ID (measured on Azure
	// DevOps) and Auth0 tenants (Checkly, whose PRM lists fifteen checkly:*
	// scopes and no offline_access; Auth0 documents that refresh tokens are
	// issued only for that scope). Keyed on metadata markers, never on
	// "advertises offline_access" alone: GitHub advertises it too and
	// refreshes without it, and a vendor that validates scopes may refuse an
	// unrequested one. Other providers with the same rule (Ory's `offline`,
	// IdentityServer) join here once a live connection has shown the need.
	if (isEntraIssuer(d.AS.Issuer) || isAuth0Metadata(&d.AS)) && containsFold(d.AS.ScopesSupported, "offline_access") && !containsFold(out, "offline_access") {
		out = append(out, "offline_access")
	}
	return out
}

// isAuth0Metadata reports whether the authorization server is an Auth0 tenant:
// the proprietary mfa_challenge_endpoint field, or an *.auth0.com issuer host.
func isAuth0Metadata(as *AuthServerMetadata) bool {
	if as == nil {
		return false
	}
	if strings.TrimSpace(as.MFAChallengeEndpoint) != "" {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(as.Issuer))
	if err != nil {
		return false
	}
	// Hostname(), not Host: the latter carries any explicit port, so an issuer
	// written https://tenant.auth0.com:443 would match neither test and the
	// tenant would go unrecognized — costing it the `offline_access` that is
	// the whole point of recognizing it.
	h := strings.ToLower(u.Hostname())
	return h == "auth0.com" || strings.HasSuffix(h, ".auth0.com")
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
// candidates for a canonical MCP server URL, in the order they are tried.
//
// First the server itself is asked: RFC 9728 §5.1 has it answer an
// unauthenticated request with 401 and a WWW-Authenticate header whose
// resource_metadata parameter points at the document. fleet used to send only
// a GET; Uptime Robot (and, per the MCP transport, any server that treats GET
// as the SSE stream) answers the GET with 404 and puts the pointer only on the
// 401 to a POST — the JSON-RPC initialize a real client would send — so both
// are probed, and the first pointer wins (#1006 catalog audit).
//
// Failing a pointer, the conventional well-known locations: RFC 9728 §3.1's
// path-inserted form (origin + /.well-known/oauth-protected-resource + path,
// the one Google's Workspace servers publish), then the path-APPENDED form
// (server URL + /.well-known/oauth-protected-resource — not in the RFC, but
// what Uptime Robot's pointer names and what some vendors publish), then the
// origin root.
//
// The result says how the candidates were arrived at, because Discover's
// legacy-origin fallback is only for a server that provably has no metadata:
// advertised means the server itself named the location (a 401 pointer), so a
// document that then cannot be fetched is the server's failure to surface;
// probeErr means a probe got no answer at all (timeout, reset), so absent
// well-known documents prove nothing — the pointer may have been on the
// request that never completed.
type prmLocations struct {
	candidates []string
	advertised bool
	probeErr   error
}

func locateResourceMetadata(ctx context.Context, httpClient *http.Client, canonicalServerURL string) (prmLocations, error) {
	var probeErr error
	u, perr := probeResourceMetadataPointer(ctx, httpClient, canonicalServerURL, http.MethodGet, "")
	if u != "" {
		return prmLocations{candidates: []string{u}, advertised: true}, nil
	}
	probeErr = perr
	u, perr = probeResourceMetadataPointer(ctx, httpClient, canonicalServerURL, http.MethodPost, initializeProbeBody)
	if u != "" {
		return prmLocations{candidates: []string{u}, advertised: true}, nil
	}
	if probeErr == nil {
		probeErr = perr
	}
	// Fallback: the conventional well-known locations.
	origin, oerr := originOf(canonicalServerURL)
	if oerr != nil {
		return prmLocations{}, oerr
	}
	root := origin + "/.well-known/oauth-protected-resource"
	// The path component only: CanonicalResourceURI keeps a query string
	// (https://host/mcp?tenant=x is a real shape) and it is not part of where
	// RFC 9728 §3.1 puts the document, so appending the well-known suffix to
	// the whole URL would have put it inside the query. EscapedPath keeps a
	// percent-escaped segment as the issuer spelled it.
	pu, uerr := url.Parse(canonicalServerURL)
	if uerr != nil {
		return prmLocations{}, uerr
	}
	if path := strings.TrimSuffix(pu.EscapedPath(), "/"); path != "" {
		return prmLocations{candidates: []string{root + path, origin + path + "/.well-known/oauth-protected-resource", root}, probeErr: probeErr}, nil
	}
	return prmLocations{candidates: []string{root}, probeErr: probeErr}, nil
}

// ProbeProtocolVersion is the MCP protocol revision the discovery probe's
// initialize announces. It MUST equal the revision fleet's real transport
// sends (internal/mcp's mcpProtocolVersion), so a server that routes or gates
// initialization on the offered revision answers the probe exactly as it will
// answer the connection the probe is validating. This package cannot import
// internal/mcp (it reaches back here through internal/a2a), so the two are
// held equal by TestProbeProtocolVersionMatchesTransport in internal/mcp.
const ProbeProtocolVersion = "2024-11-05"

// initializeProbeBody is the JSON-RPC initialize a real MCP client opens with;
// an unauthenticated one is what makes a spec-following server answer 401 with
// its resource_metadata pointer. Nothing in it identifies a user.
const initializeProbeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + ProbeProtocolVersion + `","capabilities":{},"clientInfo":{"name":"fleet","version":"discovery-probe"}}}`

// mcpSessionHeader is the Streamable HTTP transport's session header. A server
// that accepts an unauthenticated initialize may allocate a session and name
// it here; the probe wanted only the 401's pointer, so it ends any session it
// opened (the transport's DELETE with the header) rather than leave one per
// discovery for the vendor to expire.
const mcpSessionHeader = "Mcp-Session-Id"

// probeResourceMetadataPointer sends one unauthenticated request to the MCP
// server and returns the resource_metadata URL from a 401's WWW-Authenticate,
// or "" when the server answered anything else. The error is non-nil only
// when no answer was obtained (a transport failure: timeout, reset, refused
// dial; or a URL that cannot be requested at all) — that is not "no pointer",
// and Discover treats it as a reason not to assume the server has no metadata. Only the status and
// headers are read: a 2xx to an unauthenticated initialize may be an SSE
// stream the server holds open, and draining it would wait for EOF. The body
// is closed, not drained, and if the server opened a session (2xx with
// Mcp-Session-Id) it is terminated best-effort after that close.
func probeResourceMetadataPointer(ctx context.Context, httpClient *http.Client, serverURL, method, body string) (string, error) {
	// serverURL is the operator-typed MCP URL; CanonicalResourceURI has already
	// refused a non-http(s) scheme, userinfo and a hostless URL before Discover
	// is reached, and SafeHTTPClient resolves-then-dials past blocked IPs and
	// refuses redirects. Refusing the scheme again by name here costs one line
	// and keeps that argument local to the request site (see fetchJSON).
	if err := requireHTTPScheme(serverURL); err != nil {
		return "", err
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, serverURL, rd)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if sid := strings.TrimSpace(resp.Header.Get(mcpSessionHeader)); sid != "" && body != "" {
			terminateProbeSession(ctx, httpClient, serverURL, sid)
		}
		return "", nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return "", nil
	}
	// A server may send several WWW-Authenticate field lines (RFC 9110
	// §11.6.1); the pointer can sit on any of them, not only the first.
	for _, h := range resp.Header.Values("WWW-Authenticate") {
		if u := parseResourceMetadataURL(h); u != "" {
			return u, nil
		}
	}
	return "", nil
}

// probeSessionTerminateTimeout bounds the best-effort session-termination
// DELETE on its own: its outcome is ignored, so its latency must not be able
// to hold an Add for the client's full timeout, or forever on a client
// without one. A var so tests can shorten it.
var probeSessionTerminateTimeout = 5 * time.Second

// terminateProbeSession sends the Streamable HTTP session-termination DELETE
// for a session the discovery probe's initialize opened. Best-effort: a
// server may answer 405 (termination not supported) or anything else, and the
// probe's outcome does not depend on it — nor, thanks to the child deadline,
// on how long the server takes to answer.
func terminateProbeSession(ctx context.Context, httpClient *http.Client, serverURL, sessionID string) {
	ctx, cancel := context.WithTimeout(ctx, probeSessionTerminateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, serverURL, nil)
	if err != nil {
		return
	}
	req.Header.Set(mcpSessionHeader, sessionID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close() // status is all that matters, and only for the log line it does not get
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

// authServerMetadataCandidates lists the well-known URLs an issuer's metadata
// may live at, in the order the MCP authorization spec says to try them.
//
// For an issuer with no path component there are two: RFC 8414 §3.1's
// `/.well-known/oauth-authorization-server` and OpenID Connect Discovery's
// `/.well-known/openid-configuration`, both at the origin.
//
// For an issuer WITH a path (https://as.example.com/tenant1) the two specs
// disagree on where the path goes. RFC 8414 §3.1 INSERTS the well-known
// segment between host and path
// (https://as.example.com/.well-known/oauth-authorization-server/tenant1);
// OIDC Discovery 1.0 §4 APPENDS it
// (https://as.example.com/tenant1/.well-known/openid-configuration). The MCP
// spec has clients try RFC 8414 insertion, then OIDC insertion, then OIDC
// appending. fleet used to try only the two appended forms, which is why
// Stripe, Datadog, Grafana, Airtable, Monday, Mixpanel, Meta and eight more
// vendors whose issuer carries a path — and who publish, as the RFC says, at
// the inserted location only — could not be added at all (#1006 catalog
// audit). The appended RFC 8414 form is kept last: it is not in either spec
// but was what fleet asked for first until now, so a vendor that answered it
// keeps working.
func authServerMetadataCandidates(issuer string) []string {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return []string{
			issuer + "/.well-known/oauth-authorization-server",
			issuer + "/.well-known/openid-configuration",
		}
	}
	origin := u.Scheme + "://" + u.Host
	// EscapedPath, not Path: url.Parse decodes percent-escapes into Path, so
	// an issuer path segment carrying %2F or %3F would be re-emitted as a
	// path separator or a query delimiter and every candidate would name a
	// different URL than the issuer's own.
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}
	}
	return []string{
		origin + "/.well-known/oauth-authorization-server" + path, // RFC 8414 §3.1, path inserted
		origin + "/.well-known/openid-configuration" + path,       // OIDC, path inserted (MCP spec order)
		origin + path + "/.well-known/openid-configuration",       // OIDC Discovery 1.0 §4, path appended
		origin + path + "/.well-known/oauth-authorization-server", // appended RFC 8414 form: fleet's historical first try
	}
}

// fetchAuthServerMetadata fetches the issuer's RFC 8414 / OIDC discovery
// document from the first candidate location (authServerMetadataCandidates)
// that parses with both an authorization and a token endpoint AND passes
// verifyAuthServer — the issuer it claims is the one the PRM named, and PKCE
// S256 is offered. Verifying inside the loop matters now that the inserted
// forms are asked before the appended ones: a vendor whose catch-all answers
// `/.well-known/oauth-authorization-server/<path>` with its origin-level
// document (issuer = origin, not the path) would otherwise be taken at its
// first, wrong word and the valid appended document never asked for, failing
// an Add that used to work. A candidate that fails verification is recorded
// and skipped like a 404. The error names every location tried and why each
// was rejected, so an operator reading a failed Add sees which well-known
// URLs the vendor 404ed or answered with the wrong document rather than only
// the last one.
func fetchAuthServerMetadata(ctx context.Context, httpClient *http.Client, issuer string) (*AuthServerMetadata, error) {
	return fetchAuthServerMetadataOpts(ctx, httpClient, issuer, true)
}

// errIssuerMismatch is the verifyAuthServer failure a document earns by naming
// an issuer other than the URL it was fetched from — the one failure that
// confirmProxiedIssuer may turn into an acceptance.
var errIssuerMismatch = errors.New("authorization-server issuer mismatch")

// fetchAuthServerMetadataOpts is fetchAuthServerMetadata with the proxied-issuer
// confirmation switchable: the confirmation itself fetches the claimed issuer's
// document with it OFF, so a chain of documents each naming another issuer
// cannot recurse.
func fetchAuthServerMetadataOpts(ctx context.Context, httpClient *http.Client, issuer string, confirmProxied bool) (*AuthServerMetadata, error) {
	// The issuer AS WRITTEN, kept for confirmProxiedIssuer: trimming trailing
	// slashes is right for building candidate locations and for the strict
	// issuer check, but "https://proxy.example//" trimmed to its bare origin
	// would hand a tenant-scoped URL the same-origin leg. Confirmation decides
	// tenant scope, so it must see the spelling the resource actually gave.
	issuerAsWritten := strings.TrimSpace(issuer)
	issuer = strings.TrimRight(issuerAsWritten, "/")
	if issuer == "" {
		return nil, fmt.Errorf("empty authorization server issuer")
	}
	var tried []string
	// Documents that parsed but named another issuer, kept in candidate order
	// for the last-resort confirmation below. A candidate that passes the
	// strict check anywhere in the order always wins over a mismatched one
	// earlier in it: a host whose catch-all answers the inserted forms with
	// its origin-level document must not have that document taken at its word
	// while the issuer-specific one waits at the appended location.
	type mismatched struct {
		url string
		doc AuthServerMetadata
	}
	var proxied []mismatched
	for _, c := range authServerMetadataCandidates(issuer) {
		var as AuthServerMetadata
		if err := fetchJSON(ctx, httpClient, c, &as); err != nil {
			tried = append(tried, err.Error())
			continue
		}
		if as.TokenEndpoint == "" || as.AuthorizationEndpoint == "" {
			tried = append(tried, fmt.Sprintf("authorization-server metadata at %s missing token/authorization endpoint", c))
			continue
		}
		if err := verifyAuthServer(issuer, &as); err != nil {
			tried = append(tried, fmt.Sprintf("authorization-server metadata at %s: %v", c, err))
			if confirmProxied && errors.Is(err, errIssuerMismatch) {
				proxied = append(proxied, mismatched{url: c, doc: as})
			}
			continue
		}
		return &as, nil
	}
	// One fetch per claimed issuer, shared across the documents that name it:
	// the confirmation below asks the claimed issuer for its own metadata, and
	// several candidate locations can name the same one.
	ownDocs := map[string]*AuthServerMetadata{}
	ownErrs := map[string]error{}
	resolveOwn := func(claimed string) (*AuthServerMetadata, error) {
		if doc, ok := ownDocs[claimed]; ok {
			return doc, ownErrs[claimed]
		}
		doc, derr := fetchAuthServerMetadataOpts(ctx, httpClient, claimed, false)
		ownDocs[claimed], ownErrs[claimed] = doc, derr
		return doc, derr
	}
	// EVERY mismatched document gets its own confirmation attempt — there is no
	// dedupe here on purpose. Two candidate locations can serve different
	// documents under the same issuer (a catch-all copy that cannot confirm,
	// and the real one at the issuer-specific location), and any key narrower
	// than "everything confirmation looks at" lets the first suppress the
	// second — the same candidate-order trap the strict loop above exists to
	// avoid. Such a key is also a standing liability: an issuer-only key missed
	// differing endpoints, an endpoint key would still miss
	// code_challenge_methods_supported, and the next field confirmation learns
	// to read would silently break it again. The list is at most four
	// documents, and the fetch a dedupe would have saved is already saved by
	// resolveOwn, so trying all of them costs nothing.
	for _, m := range proxied {
		confirmed, cerr := confirmProxiedIssuer(issuerAsWritten, &m.doc, resolveOwn)
		if cerr == nil {
			return confirmed, nil
		}
		tried = append(tried, fmt.Sprintf("the issuer named by the document at %s did not confirm it: %v", m.url, cerr))
	}
	return nil, fmt.Errorf("fetch authorization-server metadata for %s: %s", issuer, strings.Join(tried, "; "))
}

// confirmProxiedIssuer handles the vendor pattern the #1006 catalog audit met
// five times: the protected-resource metadata names the MCP host as its
// authorization server, and that host serves a document whose `issuer` is
// some other URL. RFC 8414 §3.3 says the issuer must equal the URL the
// document was fetched from, and fleet enforced exactly that, so none of the
// five could be added. Three shapes were measured:
//
//   - a COPY of the real server's document (DocuSign → account.docusign.com,
//     Chargebee → its origin): the claimed issuer's own metadata says the same
//     endpoints;
//   - a PROXY (ZoomInfo): the issuer string is Okta's, but every endpoint is
//     on the MCP host itself, which forwards to Okta;
//   - a HYBRID (Sprout Social; OVHcloud, whose claimed issuer publishes no
//     metadata at all): authorize/token are the real issuer's, registration is
//     the MCP host's own addition.
//
// The copy is accepted when EVERY endpoint it names is either confirmed by
// the claimed issuer's own metadata (fetched from the claimed issuer's
// well-known location, with this confirmation off so documents cannot chain)
// or on the same origin as the URL the copy was fetched from — the host the
// protected-resource metadata itself trusted. Neither leg extends trust the
// plain path lacks: a PRM may name the claimed issuer directly, and the
// fetched-from host could have published a compliant document with its own
// endpoints. What both legs refuse is the actual mix-up — an endpoint that
// belongs to neither party. The validated copy is what fleet then dials (a
// proxy's endpoints are the ones its registered clients work with); its
// issuer is recorded as the identity the vendor asserts.
func confirmProxiedIssuer(fetchedFrom string, copyDoc *AuthServerMetadata, resolveOwn func(string) (*AuthServerMetadata, error)) (*AuthServerMetadata, error) {
	claimed := strings.TrimRight(strings.TrimSpace(copyDoc.Issuer), "/")
	u, err := url.Parse(claimed)
	// No userinfo either: the claimed issuer is dialed for its own metadata,
	// and net/http would turn userinfo into an Authorization header on that
	// request (see endpointCarriesUserinfo).
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("claimed issuer %q is not a plain http(s) URL", redactURLUserinfo(copyDoc.Issuer))
	}
	// Parse the authorization-server URL AS WRITTEN. Trimming trailing slashes
	// first would collapse "https://as.example//" — a distinct routed path —
	// into the bare origin and hand a tenant-scoped URL the same-origin leg,
	// the literal-slash twin of the "/%2F" decode trap scopedToOneTenant
	// exists for.
	fetchedFrom = strings.TrimSpace(fetchedFrom)
	fu, ferr := url.Parse(fetchedFrom)
	if ferr != nil {
		return nil, fmt.Errorf("authorization server %q is not a URL: %w", redactURLUserinfo(fetchedFrom), ferr)
	}
	if sameIssuerIdentity(claimed, fetchedFrom) {
		return nil, fmt.Errorf("claimed issuer is the fetched URL; nothing to confirm")
	}
	if err := verifyPKCE(copyDoc); err != nil {
		return nil, err
	}
	// The same-origin leg is open only when the resource named a BARE host as
	// its authorization server. A host scoped by anything — a path
	// (https://as.example.com/tenantA), a query (https://as.example.com?tenant=A),
	// a fragment, userinfo — may be one tenant among many that share the
	// origin, and authServerMetadataCandidates keeps only scheme, host and
	// path, so a query- or fragment-scoped tenant would otherwise be read as
	// the whole origin and get a leg that admits a sibling's endpoints. For
	// any of those, only the claimed issuer's own document may vouch.
	bareOrigin := !scopedToOneTenant(fu)
	if !bareOrigin {
		// Closing the same-origin leg is not enough on its own: a SIBLING
		// tenant (https://as.example.com/tenantB) publishes a self-consistent
		// document of its own, so confirmedBy would vouch for it and the user
		// would be sent through the wrong tenant's authorization endpoint.
		// The one document a scoped URL may be confirmed by is its own
		// ORIGIN-level one — the measured Chargebee shape, where the origin
		// answers every path with the document whose issuer IS the origin.
		// The origin fallback is the PATH-scoped Chargebee shape and nothing
		// else. authServerMetadataCandidates keeps only scheme, host and path,
		// so a query-, fragment- or userinfo-scoped URL has its scoping
		// dropped before any fetch happens: the mismatched document and the
		// claimed issuer's own metadata then come from the SAME origin-level
		// well-known location, and the origin's document self-confirms. That
		// is not an origin vouching for a tenant, it is the tenant silently
		// disappearing and being replaced by the unscoped issuer — so those
		// shapes get no fallback at all.
		if fu.RawQuery != "" || fu.ForceQuery || fu.Fragment != "" || fu.User != nil {
			return nil, fmt.Errorf("authorization server %s is scoped by something the well-known lookup drops (query, fragment or userinfo), so no document can confirm another issuer for it; this one claims %s", redactURLUserinfo(fetchedFrom), claimed)
		}
		if !sameIssuerIdentity(claimed, normalizedOrigin(fu)) {
			return nil, fmt.Errorf("authorization server %s is scoped to one tenant, so only its own origin may vouch for a document naming another issuer; this one claims %s", redactURLUserinfo(fetchedFrom), claimed)
		}
	}
	own, ownErr := resolveOwn(claimed)
	confirmedBy := func(copyEP, ownEP string) bool {
		return own != nil && ownEP != "" && sameEndpointURL(copyEP, ownEP)
	}
	// Whether the TOKEN endpoint turned out to be the claimed issuer's own, as
	// opposed to one of the proxy's. It decides whose
	// token_endpoint_auth_methods_supported the caller gets; see the end of
	// this function.
	tokenIsIssuersOwn := false
	var ownAuthz, ownToken, ownReg, ownRevoke string
	if own != nil {
		ownAuthz, ownToken, ownReg, ownRevoke = own.AuthorizationEndpoint, own.TokenEndpoint, own.RegistrationEndpoint, own.RevocationEndpoint
	}
	for _, ep := range []struct{ name, copy, own string }{
		{"authorization_endpoint", copyDoc.AuthorizationEndpoint, ownAuthz},
		{"token_endpoint", copyDoc.TokenEndpoint, ownToken},
		{"registration_endpoint", copyDoc.RegistrationEndpoint, ownReg},
		{"revocation_endpoint", copyDoc.RevocationEndpoint, ownRevoke},
	} {
		if ep.copy == "" {
			continue
		}
		if endpointCarriesUserinfo(ep.copy) {
			return nil, fmt.Errorf("%s %q embeds userinfo, which would become an Authorization header fleet never chose to send", ep.name, redactURLUserinfo(ep.copy))
		}
		if confirmedBy(ep.copy, ep.own) {
			if ep.name == "token_endpoint" {
				tokenIsIssuersOwn = true
			}
			continue
		}
		if bareOrigin && sameEndpointOrigin(ep.copy, fetchedFrom) {
			continue
		}
		// ep.own is redacted too: verifyAuthServer checks the issuer and PKCE,
		// not endpoint userinfo, so the claimed issuer's OWN document can carry
		// a credential into this message just as the copy can.
		reason := fmt.Sprintf("the claimed issuer's own metadata says %q", redactURLUserinfo(ep.own))
		if own == nil {
			reason = fmt.Sprintf("the claimed issuer publishes no metadata (%v)", ownErr)
		}
		where := "on " + redactURLUserinfo(fetchedFrom)
		if !bareOrigin {
			where = "vouched for by the same-host rule (" + redactURLUserinfo(fetchedFrom) + " names a path, so only its issuer may vouch)"
		}
		return nil, fmt.Errorf("%s %q is neither %s nor confirmed by the claimed issuer %s: %s", ep.name, redactURLUserinfo(ep.copy), where, claimed, reason)
	}
	out := *copyDoc
	out.Issuer = claimed
	if tokenIsIssuersOwn {
		// The token endpoint turned out to be the claimed issuer's own, so the
		// claimed issuer's document — not the copy — is the authority on how
		// to authenticate there. Callers read
		// token_endpoint_auth_methods_supported for three decisions
		// (PublicClientAllowed at add time, the confidential registration
		// fallback, and Basic vs post at the token endpoint itself), so a copy
		// that omits the field or advertises "none" against an endpoint whose
		// owner requires a secret would open a secretless client and fail the
		// exchange after the consent screen. An omitted list is adopted as
		// readily as a populated one: RFC 8414 §2 gives it the meaning
		// "client_secret_basic", which is exactly the claim being made.
		//
		// A PROXY's token endpoint is NOT the issuer's (it is confirmed by the
		// same-origin leg, which does not set this flag), and there the copy's
		// own list is the correct one and is kept — a proxy's registered
		// clients authenticate to the proxy.
		out.TokenEndpointAuthMethodsSupported = own.TokenEndpointAuthMethodsSupported
		// The same reasoning reaches two fields that describe the ISSUER
		// rather than how to reach it, and that a trimmed copy may simply
		// leave out. Both feed RequestedScopes, and losing either costs the
		// connection its refresh token — the exact failure F6 exists to fix:
		//   - scopes_supported, where `offline_access` is advertised;
		//   - mfa_challenge_endpoint, the only marker of an Auth0 tenant
		//     behind a custom domain (Checkly's auth.checklyhq.com — the
		//     *.auth0.com host test does not see it).
		// Filled in only where the copy is SILENT: a copy that names its own
		// scopes is making a claim about what it accepts, and a proxy may
		// legitimately offer fewer than the issuer behind it, so a populated
		// list is never overwritten.
		if len(out.ScopesSupported) == 0 {
			out.ScopesSupported = own.ScopesSupported
		}
		if strings.TrimSpace(out.MFAChallengeEndpoint) == "" {
			out.MFAChallengeEndpoint = own.MFAChallengeEndpoint
		}
	}
	return &out, nil
}

// sameEndpointOrigin reports whether an endpoint sits on the same origin as the
// URL the resource named as its authorization server, comparing CANONICAL
// origins. sameOrigin compares raw scheme://host, and url.Parse normalizes
// neither host case nor the scheme's default port, so a PRM spelling its
// authorization server "https://MCP.vendor.example" or
// "https://mcp.vendor.example:443" while its metadata uses the plain form would
// fail this leg — and with it a legitimate proxy-shaped document that no other
// leg can accept, since the claimed issuer does not vouch for proxy-local
// endpoints. CanonicalResourceURI is this package's one canonicalizer
// (lowercase scheme and host, default port dropped, userinfo refused), so both
// sides go through it rather than growing a second normalizer here.
func sameEndpointOrigin(endpoint, namedAuthServer string) bool {
	eu, eerr := url.Parse(strings.TrimSpace(endpoint))
	nu, nerr := url.Parse(strings.TrimSpace(namedAuthServer))
	if eerr != nil || nerr != nil || eu.Host == "" || nu.Host == "" {
		return false
	}
	return normalizedOrigin(eu) == normalizedOrigin(nu)
}

// sameEndpointURL compares two endpoint URLs the way a URL actually compares:
// scheme and host are case-insensitive, and everything the server routes on —
// path, query, fragment — is not. strings.EqualFold over the whole URL would
// make /oauth/token and /oauth/Token the same endpoint, so a copied document
// could point a "confirmed" endpoint at a different handler on the claimed
// issuer's host. A trailing slash on the path is still ignored, as it was.
func sameEndpointURL(a, b string) bool {
	au, aerr := url.Parse(strings.TrimSpace(a))
	bu, berr := url.Parse(strings.TrimSpace(b))
	if aerr != nil || berr != nil {
		return false
	}
	// Userinfo is part of an endpoint's identity, and endpointCarriesUserinfo
	// has already refused it outright — comparing it here keeps this helper
	// honest on its own terms rather than relying on that caller.
	if (au.User == nil) != (bu.User == nil) || (au.User != nil && au.User.String() != bu.User.String()) {
		return false
	}
	return normalizedOrigin(au) == normalizedOrigin(bu) &&
		trimOneTrailingSlash(au.EscapedPath()) == trimOneTrailingSlash(bu.EscapedPath()) &&
		au.RawQuery == bu.RawQuery &&
		// ForceQuery is the bare "?" of https://as.example/token? — an empty
		// RawQuery either way, but Go puts the "?" on the wire, so the two
		// are different request targets to anything that routes on the raw
		// target. Two URLs that reach different handlers are not one endpoint.
		au.ForceQuery == bu.ForceQuery &&
		au.Fragment == bu.Fragment
}

// sameIssuerIdentity reports whether two authorization-server URLs name the
// same thing: canonical origins (so an explicit :443 or a mixed-case host does
// not split one identity in two), and the routed remainder — escaped path,
// query, fragment — compared exactly but for a trailing slash. It is the one
// answer to "are these the same authorization server" inside the confirmation
// path, so a spelling difference cannot decide whether a document confirms.
func sameIssuerIdentity(a, b string) bool {
	au, aerr := url.Parse(strings.TrimSpace(a))
	bu, berr := url.Parse(strings.TrimSpace(b))
	if aerr != nil || berr != nil {
		return false
	}
	return normalizedOrigin(au) == normalizedOrigin(bu) &&
		trimOneTrailingSlash(au.EscapedPath()) == trimOneTrailingSlash(bu.EscapedPath()) &&
		au.RawQuery == bu.RawQuery &&
		au.ForceQuery == bu.ForceQuery &&
		au.Fragment == bu.Fragment
}

// normalizedOrigin is the case- and default-port-normalized scheme://host of a
// parsed URL — the half of a URL that RFC 3986 §6.2.2 makes case-insensitive,
// with the scheme's default port dropped so https://as.example and
// https://as.example:443 are the one origin they denote. Comparing URL.Host
// raw fails that pair, which is how a copied document spelling an endpoint
// with an explicit :443 would miss confirmation by the very issuer that
// vouches for it. Nothing the server routes on — path, query — passes through
// here; those are compared exactly by the caller.
func normalizedOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	// Hostname() strips the brackets from an IPv6 literal, so putting the port
	// back with a bare colon would make https://[2001:db8::1]:8443 and
	// https://[2001:db8::1:8443] — different network endpoints — normalize to
	// the same string, and a copied endpoint would read as issuer-confirmed.
	// The brackets are what keep the host/port boundary unambiguous.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// trimOneTrailingSlash removes a SINGLE trailing slash — the one spelling
// difference (https://as.example/token vs .../token/) worth tolerating between
// two spellings of one endpoint. Trimming every trailing slash would fold
// "/token//" in with them, and a doubled slash is a path a server may route
// somewhere else entirely, which is the whole question confirmation answers.
func trimOneTrailingSlash(path string) string {
	return strings.TrimSuffix(path, "/")
}

// redactURLUserinfo replaces any credential embedded in a URL before it is
// named in an error. These errors surface to operators and logs, and AGENTS.md
// is explicit that credentials never reach either — refusing a userinfo-bearing
// URL and then printing the userinfo would leak exactly what the refusal is
// there to stop.
func redactURLUserinfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil {
		if u.User == nil {
			return raw
		}
		u.User = url.User("redacted")
		return u.String()
	}
	// Unparseable: rather than risk printing a credential, keep only what
	// follows the last "@".
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		return "redacted@" + raw[i+1:]
	}
	return raw
}

// scopedToOneTenant reports whether an authorization-server URL is scoped to a
// single tenant rather than naming a whole origin — by a path, a query, a
// fragment or userinfo. Only an ABSENT or literal-root path is bare:
// url.Parse DECODES percent-escapes into Path, so an issuer whose path is
// "/%2F" arrives as "//" and would trim away to nothing, reading as a bare
// origin while RawPath/EscapedPath keep it a distinct routed path — the same
// decode trap authServerMetadataCandidates already navigates with EscapedPath.
func scopedToOneTenant(u *url.URL) bool {
	path := u.EscapedPath()
	// ForceQuery is the bare "?" of https://as.example? — RawQuery stays empty
	// while Go still puts the delimiter on the wire, so it is a distinct request
	// target and candidate construction drops it, exactly like a query.
	return (path != "" && path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil
}

// endpointCarriesUserinfo reports whether an endpoint URL embeds userinfo
// (https://name:pw@as.example/token). fleet refuses such an endpoint rather
// than confirming it: net/http turns URL userinfo into a Basic `Authorization`
// header whenever the request does not set one itself (http.Client.send), so a
// copied document could bolt credentials of its choosing onto an endpoint the
// claimed issuer vouched for — unintended authentication on a public-client
// token exchange or a registration POST. Neither leg may admit it: sameOrigin
// compares scheme://host and would not notice either. CanonicalResourceURI
// already refuses userinfo on the resource side, so this matches it.
func endpointCarriesUserinfo(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.User != nil
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
		return fmt.Errorf("%w: metadata says %q, expected %q", errIssuerMismatch, as.Issuer, expectedIssuer)
	}
	return verifyPKCE(as)
}

// verifyPKCE: an empty methods list means the AS didn't advertise; the MCP spec
// requires S256, so we proceed assuming S256. A non-empty list that omits S256
// is a hard reject.
func verifyPKCE(as *AuthServerMetadata) error {
	if len(as.CodeChallengeMethodsSupported) == 0 {
		return nil
	}
	for _, m := range as.CodeChallengeMethodsSupported {
		if strings.EqualFold(m, "S256") {
			return nil
		}
	}
	return fmt.Errorf("authorization server does not support PKCE S256 (advertises %v)", as.CodeChallengeMethodsSupported)
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
// httpStatusError is fetchJSON's non-2xx result. It carries the status so a
// caller can tell "the document is not there" (404/410) from "the server is
// failing to serve it" (5xx, 429, …): the MCP spec's legacy-origin fallback
// applies to the first and must never be triggered by the second.
type httpStatusError struct {
	URL    string
	Status int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("GET %s: status %d", e.URL, e.Status) }

// metadataAbsent reports whether a fetchJSON failure means the document does
// not exist at that location, as opposed to an operational failure (a 5xx, a
// timeout, malformed JSON) that a modern server may recover from and that must
// surface rather than be papered over by a synthesized configuration.
func metadataAbsent(err error) bool {
	var se *httpStatusError
	return errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusGone)
}

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
		return &httpStatusError{URL: url, Status: resp.StatusCode}
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
