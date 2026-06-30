package mcpoauth

import (
	"fmt"
	"net/url"
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
// its specificity. Userinfo (credentials embedded in the URL) is rejected.
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
	port := u.Port()
	// Drop the scheme's default port so https://x:443 == https://x.
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	canonHost := host
	if port != "" {
		canonHost = host + ":" + port
	}

	out := url.URL{Scheme: scheme, Host: canonHost, Path: u.Path, RawQuery: u.RawQuery}
	// A lone root path adds no identity; drop it so https://x/ == https://x.
	if out.Path == "/" {
		out.Path = ""
	}
	return out.String(), nil
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
