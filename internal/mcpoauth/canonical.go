package mcpoauth

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalResourceURI normalizes a remote-MCP server URL into the single,
// stable identity used EVERYWHERE this server is referenced: the DB key, the
// encryption AAD, the OAuth `state` record, the RFC 8707 `resource` indicator,
// the Authorization-header attachment, and the broker routing key. Treating
// "URL-ish strings" (trailing slash, casing, default port, alias) as
// interchangeable is the classic way to leak a bearer to the wrong resource, so
// there is exactly ONE canonicalizer and everything funnels through it.
//
// Canonical form (aligned with RFC 8707 §2 / RFC 9728): lowercase scheme and
// host, default port removed, fragment removed, a lone root path "/" dropped.
// Path (and any query) are otherwise preserved so a path-scoped MCP server keeps
// its specificity — including a percent-escaped segment as the operator typed
// it: "/tenant%2Fone/mcp" stays one segment, because decoding it to
// "/tenant/one/mcp" would make fleet dial, key and name (RFC 8707 `resource`)
// a different route than the one the vendor published (#1485 follow-up).
// Escapes that are merely the default encoding of a character (%20 for a
// space) are normalized, as before. Userinfo (credentials embedded in the URL)
// is rejected.
func CanonicalResourceURI(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty server URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	if !u.IsAbs() {
		return "", fmt.Errorf("server URL must be absolute (got %q)", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("server URL scheme must be http or https (got %q)", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("server URL must not embed credentials (userinfo)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("server URL has no host")
	}
	// Drop the scheme's default port so https://x:443 == https://x, comparing
	// it NUMERICALLY — see canonicalPort.
	port := canonicalPort(scheme, u.Port())
	// Hostname() strips the brackets from an IPv6 literal, so the host and the
	// port have to be re-joined with them or the boundary between the two
	// becomes ambiguous: https://[2001:db8::1]:8443 and https://[2001:db8::1:8443]
	// are different servers that would otherwise canonicalize to the identical
	// string https://2001:db8::1:8443 — two identities collapsing into one, in
	// the very function whose job is to keep them apart. A bracketless IPv6
	// host is also not a URL Go will re-parse, so the old output could not
	// round-trip.
	canonHost := host
	if strings.Contains(canonHost, ":") {
		canonHost = "[" + canonHost + "]"
	}
	if port != "" {
		canonHost += ":" + port
	}

	// RawPath is set by url.Parse only when the operator's escaping differs
	// from Go's default encoding of the decoded path — exactly the case where
	// an escaped reserved character (%2F, %3F, %23) carries meaning the
	// decoded form loses. String() emits it when it is a valid encoding of
	// Path, and ignores it otherwise, so a stale or malformed RawPath can
	// never leak into the identity.
	out := url.URL{Scheme: scheme, Host: canonHost, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	// A lone root path adds no identity; drop it so https://x/ == https://x.
	if out.Path == "/" {
		out.Path, out.RawPath = "", ""
	}
	return out.String(), nil
}

// canonicalPort returns the port to keep for a scheme: "" when it is the
// scheme's default, else the port in its numeric form.
//
// url.URL.Port() preserves whatever spelling was written, and Go resolves
// "0443" to 443 when it dials, so comparing the string would make
// https://as.example:0443 a different identity from https://as.example while
// both reach the same endpoint — and https://as.example:08443 a different one
// from https://as.example:8443. Two identities for one server is exactly what
// this canonicalization exists to prevent. A non-numeric port is left as
// written rather than guessed at; url.Parse already rejects most of those.
func canonicalPort(scheme, port string) string {
	if port == "" {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 {
		return port
	}
	if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
		return ""
	}
	return strconv.Itoa(n)
}

// ValidateServerURL canonicalizes raw and enforces the transport-security
// policy: HTTPS only, unless allowInsecureHTTP is set (dev/test escape hatch).
// It does NOT perform the IP/SSRF check — that happens at dial time in the
// safe HTTP client (see ssrf.go), because a hostname's resolved address can
// change between save and use (DNS rebinding).
func ValidateServerURL(raw string, allowInsecureHTTP bool) (string, error) {
	canon, err := CanonicalResourceURI(raw)
	if err != nil {
		return "", err
	}
	if !allowInsecureHTTP && strings.HasPrefix(canon, "http://") {
		return "", fmt.Errorf("remote MCP server URL must use https:// (got %q)", canon)
	}
	return canon, nil
}

// originOf returns scheme://host for u — used to build the default
// .well-known/oauth-protected-resource location when a server doesn't return a
// resource_metadata pointer in its 401.
func originOf(canonical string) (string, error) {
	u, err := url.Parse(canonical)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// sameOrigin reports whether two canonical URIs share scheme://host. Used to
// gate adoption of a PRM-declared `resource` (RFC 9728 §3.3): we only trust it
// when it stays on the origin the user actually requested.
func sameOrigin(a, b string) bool {
	oa, ea := originOf(a)
	ob, eb := originOf(b)
	return ea == nil && eb == nil && oa == ob
}
