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
		as, err = fetchAuthServerMetadata(ctx, httpClient, issuer)
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
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, fmt.Errorf("empty authorization server issuer")
	}
	var tried []string
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
			continue
		}
		return &as, nil
	}
	return nil, fmt.Errorf("fetch authorization-server metadata for %s: %s", issuer, strings.Join(tried, "; "))
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
